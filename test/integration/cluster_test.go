//go:build integration

package integration

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

const (
	q6  = "给 TLS Secret 追加 ownerRef 后 cert-manager 的 SSA 是否保留它"
	q7  = "Secret 被替换为另一张合法证书时 cert-manager 是否重签并 bump revision"
	q11 = "cert-manager 打在 Secret 上的注解是否稳定存在"
)

// 三个探针依赖的集群类型。cert-manager 的两个是共同前提；AliyunCertificate 只有 #6
// 需要——它要一个真实存在的对象当 ownerRef 的目标。
var (
	kindCertificate       = cmapi.SchemeGroupVersion.WithKind(cmapi.CertificateKind)
	kindIssuer            = cmapi.SchemeGroupVersion.WithKind(cmapi.IssuerKind)
	kindAliyunCertificate = certsv1alpha1.GroupVersion.WithKind("AliyunCertificate")
)

// clusterClient 从 INTEGRATION_KUBECONFIG 建一个直读 client。缺 kubeconfig 就 skip。
// need 里的类型必须真的注册在集群上，否则同样 skip——见 requireAPIKinds。
func clusterClient(t *testing.T, id, question string, need ...schema.GroupVersionKind) client.Client {
	t.Helper()
	kubeconfig := env(EnvKubeconfig)
	if kubeconfig == "" {
		RecordSkip(t, id, question, "未设置 "+EnvKubeconfig)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("读取 kubeconfig 失败: %v", err)
	}
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatalf("注册 core scheme 失败: %v", err)
	}
	if err := cmapi.AddToScheme(sch); err != nil {
		t.Fatalf("注册 cert-manager scheme 失败: %v", err)
	}
	if err := certsv1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("注册本项目 scheme 失败: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("构造 client 失败: %v", err)
	}
	requireAPIKinds(t, c, id, question, need...)
	return c
}

// requireAPIKinds 确认这些类型确实注册在集群上，否则把这一项记成「未实测」并 skip。
//
// 为什么要这一层：scheme 里注册了 Go 类型不等于集群上装了对应的 CRD。cert-manager
// 或本项目的 CRD 缺席时，直接 Create 会抛出
// `no matches for kind "Certificate" in version "cert-manager.io/v1"`，用例变红、
// RESULTS.md 里却一个字都没有。读报告的人于是分不清两件完全不同的事：「测过了、
// cert-manager 的行为就是这样」和「前置条件没满足、根本没测成」。前置检查把后者
// 明确记成「未实测」并写清缺了哪个类型，装好 cert-manager 后重跑即可拿到真结论。
//
// 刻意不把 discovery 的原始错误写进报告：它通常带着 API server 的 URL 或 IP
// （`Get "https://…:6443/api": dial tcp …`），而集群地址不该落进日志与 RESULTS.md。
// 「缺了哪个 group/version/kind」已经是足够的线索；连不上时请自己 `oc api-resources`。
func requireAPIKinds(
	t *testing.T, c client.Client, id, question string, kinds ...schema.GroupVersionKind,
) {
	t.Helper()
	var missing, unreachable []string
	for _, gvk := range kinds {
		name := gvk.GroupVersion().String() + " " + gvk.Kind
		_, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		switch {
		case err == nil:
		case meta.IsNoMatchError(err):
			missing = append(missing, name)
		default:
			unreachable = append(unreachable, name)
		}
	}
	if len(unreachable) > 0 {
		RecordSkip(t, id, question,
			"集群 discovery 不可达，无法确认这些类型是否存在："+joinComma(unreachable)+
				"（原始错误含集群地址，刻意不记入报告）")
	}
	if len(missing) > 0 {
		RecordSkip(t, id, question,
			"集群上没有这些 API 类型："+joinComma(missing)+
				"；装好 cert-manager（与本项目 CRD）后重跑本用例即可得到结论")
	}
}

// setupIssuedCertificate 在一个新建的 namespace 里用 SelfSigned Issuer 签一张证书，
// 返回 namespace、Certificate 名与 Secret 名。用 SelfSigned 而不是 LE：这三项探针
// 只关心 cert-manager 自身的行为，走 ACME 只会消耗配额并引入 DNS 依赖。
func setupIssuedCertificate(t *testing.T, c client.Client, dnsName string) (ns, certName, secretName string) {
	t.Helper()
	ctx := context.Background()
	ns = "it-certmgr-" + randHex(t, 4)
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("创建 namespace 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	issuer := &cmapi.Issuer{
		ObjectMeta: metav1.ObjectMeta{Name: "selfsigned", Namespace: ns},
		Spec: cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{
			SelfSigned: &cmapi.SelfSignedIssuer{},
		}},
	}
	if err := c.Create(ctx, issuer); err != nil {
		t.Fatalf("创建 SelfSigned Issuer 失败: %v", err)
	}

	certName, secretName = "probe", "probe-tls"
	cert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: ns},
		Spec: cmapi.CertificateSpec{
			SecretName: secretName,
			DNSNames:   []string{dnsName},
			IssuerRef:  cmmeta.IssuerReference{Name: "selfsigned", Kind: "Issuer", Group: "cert-manager.io"},
		},
	}
	if err := c.Create(ctx, cert); err != nil {
		t.Fatalf("创建 Certificate 失败: %v", err)
	}
	waitSecret(t, c, ns, secretName)
	return ns, certName, secretName
}

// waitSecret 等 Secret 出现且含 tls.crt。SelfSigned 通常几秒内完成。
func waitSecret(t *testing.T, c client.Client, ns, name string) *corev1.Secret {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		s := &corev1.Secret{}
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, s)
		if err == nil && len(s.Data["tls.crt"]) > 0 {
			return s
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("读 Secret 失败: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 Secret %s/%s 超时", ns, name)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestSecretOwnerRefSurvivesReissue（§12.3 #6）：如果 ownerRef 被保留，operator 就
// 可以靠 GC 级联删除 Secret，从而去掉 RBAC 上的 secrets: delete 和 finalizer 里
// 「Certificate 必须先于 Secret 死」那道顺序约束。
func TestSecretOwnerRefSurvivesReissue(t *testing.T) {
	c := clusterClient(t, "#6", q6, kindCertificate, kindIssuer, kindAliyunCertificate)
	ctx := context.Background()
	ns, certName, secretName := setupIssuedCertificate(t, c, "probe6.integration.invalid")

	// 造一个真实的 AliyunCertificate 当 owner——ownerRef 必须指向存在的对象，
	// 否则 GC 会立刻把 Secret 删掉，测出来的就不是 cert-manager 的行为。
	owner := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-owner", Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{DNSNames: []string{"probe6.integration.invalid"}},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "unused"},
				Region:         "cn-hangzhou",
			},
		},
	}
	if err := c.Create(ctx, owner); err != nil {
		t.Fatalf("创建 owner AliyunCertificate 失败: %v", err)
	}

	s := waitSecret(t, c, ns, secretName)
	s.OwnerReferences = append(s.OwnerReferences, metav1.OwnerReference{
		APIVersion: certsv1alpha1.GroupVersion.String(),
		Kind:       "AliyunCertificate",
		Name:       owner.Name,
		UID:        owner.UID,
	})
	if err := c.Update(ctx, s); err != nil {
		t.Fatalf("给 Secret 加 ownerRef 失败: %v", err)
	}

	// 改 dnsNames 触发重签（spec §2.3：改 dnsNames 会重签）。
	cert := &cmapi.Certificate{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: certName}, cert); err != nil {
		t.Fatalf("读 Certificate 失败: %v", err)
	}
	cert.Spec.DNSNames = []string{"probe6.integration.invalid", "probe6b.integration.invalid"}
	if err := c.Update(ctx, cert); err != nil {
		t.Fatalf("改 Certificate 失败: %v", err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	kept := false
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		cur := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: secretName}, cur); err != nil {
			continue
		}
		if !hasSAN(t, cur.Data["tls.crt"], "probe6b.integration.invalid") {
			continue // 还没重签完
		}
		for _, or := range cur.OwnerReferences {
			if or.Kind == "AliyunCertificate" && or.Name == owner.Name {
				kept = true
			}
		}
		break
	}
	result := "不保留：重签后 ownerRef 被 SSA 抹掉"
	if kept {
		result = "保留：重签后 ownerRef 仍在"
	}
	Record(t, "#6", q6, result, "cert-manager v1.21.1，SelfSigned Issuer，改 dnsNames 触发重签")
}

// TestSecretReplacementBumpsRevision（§12.3 #7）：operator 不缓存 Secret，靠 1h
// resync 兜底。如果 cert-manager 会因为 Secret 被换掉而重签并 bump revision，
// 盲区就小得多——watch Certificate 就能第一时间知道。
func TestSecretReplacementBumpsRevision(t *testing.T) {
	c := clusterClient(t, "#7", q7, kindCertificate, kindIssuer)
	ctx := context.Background()
	ns, certName, secretName := setupIssuedCertificate(t, c, "probe7.integration.invalid")

	before := &cmapi.Certificate{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: certName}, before); err != nil {
		t.Fatalf("读 Certificate 失败: %v", err)
	}
	beforeRev := revisionOf(before)

	// 换成另一张「同样合法、SAN 也满足 spec」的证书，但不是 cert-manager 签的那张。
	other := selfSignedFor(t, "probe7.integration.invalid")
	s := waitSecret(t, c, ns, secretName)
	s.Data["tls.crt"] = other.certPEM
	s.Data["tls.key"] = other.keyPEM
	if err := c.Update(ctx, s); err != nil {
		t.Fatalf("替换 Secret 内容失败: %v", err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	bumped := false
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		cur := &cmapi.Certificate{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: certName}, cur); err != nil {
			continue
		}
		if revisionOf(cur) > beforeRev {
			bumped = true
			break
		}
	}
	result := "不重签：3 分钟内 revision 未变，盲区只能靠 resync 兜住"
	if bumped {
		result = "重签并 bump revision：watch Certificate 即可发现"
	}
	Record(t, "#7", q7, result, "替换前 revision="+itoa(int64(beforeRev)))
}

// TestCertManagerSecretAnnotations（§12.3 #11）：SecretNameConflict 的判定要靠这些
// 注解认出「这个 Secret 是不是我们那张 Certificate 的产物」。
func TestCertManagerSecretAnnotations(t *testing.T) {
	c := clusterClient(t, "#11", q11, kindCertificate, kindIssuer)
	ns, _, secretName := setupIssuedCertificate(t, c, "probe11.integration.invalid")
	s := waitSecret(t, c, ns, secretName)

	want := []string{
		"cert-manager.io/certificate-name",
		"cert-manager.io/issuer-name",
		"cert-manager.io/issuer-kind",
		"cert-manager.io/issuer-group",
	}
	var missing []string
	for _, k := range want {
		if s.Annotations[k] == "" {
			missing = append(missing, k)
		}
	}
	result := "四个注解全部存在，可作为 SecretNameConflict 的判定依据"
	detail := "certificate-name=" + s.Annotations["cert-manager.io/certificate-name"]
	if len(missing) > 0 {
		result = "缺失：" + joinComma(missing)
		t.Errorf("cert-manager 没打这些注解: %v", missing)
	}
	Record(t, "#11", q11, result, detail)
}

type pemPair struct{ certPEM, keyPEM []byte }

// selfSignedFor 生成一张覆盖 dnsName 的自签证书，用来替换 Secret 内容。
func selfSignedFor(t *testing.T, dnsName string) pemPair {
	t.Helper()
	certPEM, keyPEM := testutil.SelfSigned(t, dnsName)
	return pemPair{certPEM: certPEM, keyPEM: keyPEM}
}

// hasSAN 判断 PEM 里的第一张证书是否覆盖 name。
func hasSAN(t *testing.T, certPEM []byte, name string) bool {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return false
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return false
	}
	for _, n := range c.DNSNames {
		if n == name {
			return true
		}
	}
	return false
}

// revisionOf 读 status.revision；未签发时为 0。
func revisionOf(c *cmapi.Certificate) int {
	if c.Status.Revision == nil {
		return 0
	}
	return *c.Status.Revision
}

func joinComma(v []string) string { return strings.Join(v, ", ") }
