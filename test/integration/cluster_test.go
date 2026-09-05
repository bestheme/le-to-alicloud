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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/pki/testutil"
)

const (
	q6  = "给 TLS Secret 追加 ownerRef 后 cert-manager 的 SSA 是否保留它"
	q7  = "Secret 被替换为另一张合法证书时 cert-manager 是否重签并 bump revision"
	q11 = "cert-manager 打在 Secret 上的注解是否稳定存在"
)

// selfSignedIssuer 是三个探针共用的 Issuer 名。它是**namespace 内**的 SelfSigned
// Issuer：这三项只关心 cert-manager 自身的行为，走 ACME 只会消耗 Let's Encrypt
// 配额并引入 DNS 依赖。全文不出现 ClusterIssuer。
const selfSignedIssuer = "selfsigned"

// selfSignedIssuerRef 指向上面那个 Issuer。凡是探针创建的、会被 cert-manager 或
// 本项目 operator 消费的对象都必须显式带上它——留空会回落到 operator 的
// --default-issuer-*（本仓库的默认值是 letsencrypt-prod / ClusterIssuer），
// 那就等于让探针去开一个真实的 ACME order。
var selfSignedIssuerRef = cmmeta.IssuerReference{
	Name:  selfSignedIssuer,
	Kind:  cmapi.IssuerKind,
	Group: cmapi.SchemeGroupVersion.Group,
}

// 三个探针依赖的集群类型。cert-manager 的两个是共同前提；AliyunCertificate 只有 #6
// 需要——它要一个真实存在的对象当 ownerRef 的目标。
var (
	kindCertificate       = cmapi.SchemeGroupVersion.WithKind(cmapi.CertificateKind)
	kindIssuer            = cmapi.SchemeGroupVersion.WithKind(cmapi.IssuerKind)
	kindAliyunCertificate = certsv1alpha1.GroupVersion.WithKind("AliyunCertificate")
)

// probe 把「当前测的是哪一条待核实问题」沿调用链带下去。
//
// 为什么需要它：设置阶段（建 namespace / Issuer / Certificate、等 Secret）一旦失败，
// 只 t.Fatalf 而不登记的话，这一项会从 RESULTS.md 里**整行消失**——writeResults 是
// 整文件覆盖，而「保留既有文件」的守卫只在本次运行的 finding **全部**被 skip 时才触发。
// 一次「CAS 探针成功、集群探针建 namespace 失败」的运行会产出一份 #6 根本不存在的报告，
// 读的人无法把它与「这条问题从来不在清单上」区分开。记一条「未实测」永远比消失好。
type probe struct {
	id       string
	question string
}

var (
	probeOwnerRef    = probe{id: "#6", question: q6}
	probeRevision    = probe{id: "#7", question: q7}
	probeAnnotations = probe{id: "#11", question: q11}
)

func (p probe) skip(t *testing.T, why string) {
	t.Helper()
	RecordSkip(t, p.id, p.question, why)
}

func (p probe) record(t *testing.T, result, detail string) {
	t.Helper()
	Record(t, p.id, p.question, result, detail)
}

// errBrief 把一个集群错误压成一句**不含集群地址**的话。
//
// k8s 的 REST client 在传输层失败时返回 *url.Error，用 %v 渲染出来是
// `Get "https://api.<cluster>:6443/…": dial tcp 10.x.x.x:6443: …`——API server 的
// 主机名与内网 IP 就这么进了测试日志。scrub 拦不住：它只认三个阿里云环境变量与 PEM
// 私钥块，而且根本不作用于 t.Fatalf / t.Logf 的参数。所以集群错误一律先过这里。
//
// API 层的错误保留 reason（Forbidden / NotFound / Conflict / Invalid / Timeout …）——
// 它是排障最有用的一位信息，且来自 Status 的枚举字段而不是自由文本，不会夹带地址；
// 刻意不带 Status.Message，那是 API 响应体。传输层错误整段丢弃。
func errBrief(err error) string {
	if err == nil {
		return "无错误"
	}
	if r := apierrors.ReasonForError(err); r != metav1.StatusReasonUnknown {
		return "API 错误 reason=" + string(r)
	}
	return "传输层错误（原文含集群地址与 IP，刻意不记录；请自行用 oc 复现）"
}

// clusterClient 从 INTEGRATION_KUBECONFIG 建一个直读 client。缺 kubeconfig 就 skip。
// need 里的类型必须真的注册在集群上，否则同样 skip——见 requireAPIKinds。
func clusterClient(t *testing.T, p probe, need ...schema.GroupVersionKind) client.Client {
	t.Helper()
	kubeconfig := env(EnvKubeconfig)
	if kubeconfig == "" {
		p.skip(t, "未设置 "+EnvKubeconfig)
	}
	// 下面两处刻意不回显 err：kubeconfig 的解析错误会回显文件内容（含 server 地址与
	// token），client 构造错误可能带 endpoint。环境变量名已经足够定位问题。
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("读取 kubeconfig 失败（原文可能回显文件内容，刻意不记录）；请检查 %s 指向的文件", EnvKubeconfig)
	}
	sch := runtime.NewScheme()
	// 这三处是纯本地的类型注册，失败只可能是代码 bug，原文不含任何集群信息。
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
		t.Fatalf("构造 client 失败（原文可能含集群地址，刻意不记录）；请检查 %s", EnvKubeconfig)
	}
	requireAPIKinds(t, c, p, need...)
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
// 检查发生在创建任何集群资源之前，所以前提不满足时集群上一个对象都不会留下。
func requireAPIKinds(t *testing.T, c client.Client, p probe, kinds ...schema.GroupVersionKind) {
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
		p.skip(t, "集群 discovery 不可达，无法确认这些类型是否存在："+joinComma(unreachable)+
			"（原始错误含集群地址，刻意不记入报告）")
	}
	if len(missing) > 0 {
		p.skip(t, "集群上没有这些 API 类型："+joinComma(missing)+
			"；装好 cert-manager（与本项目 CRD）后重跑本用例即可得到结论")
	}
}

// certManagerVersion 读 certificates.cert-manager.io 这个 CRD 上的
// app.kubernetes.io/version label，也就是**集群上真正跑着的** cert-manager 版本。
//
// 不要用 go.mod 里钉的 github.com/cert-manager/cert-manager 版本充数：那是客户端库
// 的版本，与集群里装的哪个 release 毫无关系。SSA 是否保留 ownerRef 是版本敏感的行为，
// 而 #6 的结论会经 Task 14 写进 spec——把观测归因到一个从未运行过的版本是伪造证据。
// 读不到（没权限、CRD 没这个 label）就如实返回「版本未知」。
func certManagerVersion(t *testing.T, c client.Client) string {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
	})
	key := client.ObjectKey{Name: "certificates." + cmapi.SchemeGroupVersion.Group}
	if err := c.Get(context.Background(), key, crd); err != nil {
		t.Logf("读 cert-manager CRD 版本失败（%s），证据里记为版本未知", errBrief(err))
		return "cert-manager 版本未知"
	}
	if v := crd.GetLabels()["app.kubernetes.io/version"]; v != "" {
		return "cert-manager " + v
	}
	return "cert-manager 版本未知"
}

// setupIssuedCertificate 在一个新建的 namespace 里用 SelfSigned Issuer 签一张证书，
// 返回 namespace、Certificate 名与 Secret 名。设置阶段的失败一律记成「未实测」并 skip，
// 理由见 probe 的注释。
func setupIssuedCertificate(t *testing.T, c client.Client, p probe, dnsName string) (ns, certName, secretName string) {
	t.Helper()
	ctx := context.Background()
	ns = "it-certmgr-" + randHex(t, 4)
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		p.skip(t, "创建测试 namespace 失败："+errBrief(err))
	}
	t.Cleanup(func() {
		// 删不掉必须留下痕迹：这个集群已经有一个卡了半年的 Terminating namespace，
		// 再静默泄漏一个只会让人更难查。清理失败不 Fail——结论已经拿到了。
		err := c.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("删除 namespace %s 失败，需人工清理（%s）", ns, errBrief(err))
		}
	})

	issuer := &cmapi.Issuer{
		ObjectMeta: metav1.ObjectMeta{Name: selfSignedIssuer, Namespace: ns},
		Spec: cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{
			SelfSigned: &cmapi.SelfSignedIssuer{},
		}},
	}
	if err := c.Create(ctx, issuer); err != nil {
		p.skip(t, "创建 SelfSigned Issuer 失败："+errBrief(err))
	}

	certName, secretName = "probe", "probe-tls"
	cert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: ns},
		Spec: cmapi.CertificateSpec{
			SecretName: secretName,
			DNSNames:   []string{dnsName},
			IssuerRef:  selfSignedIssuerRef,
		},
	}
	if err := c.Create(ctx, cert); err != nil {
		p.skip(t, "创建 Certificate 失败："+errBrief(err))
	}
	waitSecret(t, c, p, ns, secretName)
	return ns, certName, secretName
}

// waitSecret 等 Secret 出现且含 tls.crt。SelfSigned 通常几秒内完成。
//
// 读失败不再立刻 Fatal：Forbidden / 传输抖动都可能是一次性的，而一旦 Fatal，这一项就
// 从报告里消失了。改成记住最后一次失败、继续轮询，到点仍拿不到就记「未实测」并 skip。
func waitSecret(t *testing.T, c client.Client, p probe, ns, name string) *corev1.Secret {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	lastErr := ""
	for {
		s := &corev1.Secret{}
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, s)
		if err == nil && len(s.Data["tls.crt"]) > 0 {
			return s
		}
		if err != nil && !apierrors.IsNotFound(err) {
			lastErr = errBrief(err)
		}
		if time.Now().After(deadline) {
			why := "等待 Secret " + name + " 被签发超时（2 分钟），cert-manager 没有把 tls.crt 写出来"
			if lastErr != "" {
				why += "；最后一次读取失败：" + lastErr
			}
			p.skip(t, why)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitRevision 等 status.revision 出现并返回它。
//
// #7 必须在这之后才捕获基线：cert-manager 是**先写 Secret data、后 patch
// status.revision** 的，而 waitSecret 只要 tls.crt 非空就返回。如果在这个窗口里读，
// 基线会是 0，随后**首次签发**留下的 revision=1 就会被判成「重签了」——一个不存在的
// 重签被写进 RESULTS.md。宁可 skip 也不要这种假阳性。
func waitRevision(t *testing.T, c client.Client, p probe, ns, certName string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	lastErr := ""
	for {
		cur := &cmapi.Certificate{}
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: certName}, cur)
		switch {
		case err != nil:
			lastErr = errBrief(err)
		case cur.Status.Revision != nil:
			return *cur.Status.Revision
		}
		if time.Now().After(deadline) {
			why := "首次签发的 status.revision 2 分钟内一直没出现，继续测会把首签的 revision 误判成重签"
			if lastErr != "" {
				why += "；最后一次读取失败：" + lastErr
			}
			p.skip(t, why)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestSecretOwnerRefSurvivesReissue（§12.3 #6）：如果 ownerRef 被保留，operator 就
// 可以靠 GC 级联删除 Secret，从而去掉 RBAC 上的 secrets: delete 和 finalizer 里
// 「Certificate 必须先于 Secret 死」那道顺序约束。
func TestSecretOwnerRefSurvivesReissue(t *testing.T) {
	p := probeOwnerRef
	c := clusterClient(t, p, kindCertificate, kindIssuer, kindAliyunCertificate)
	ctx := context.Background()
	const (
		san1 = "probe6.integration.invalid"
		san2 = "probe6b.integration.invalid"
	)
	ns, certName, secretName := setupIssuedCertificate(t, c, p, san1)
	secretKey := client.ObjectKey{Namespace: ns, Name: secretName}

	// 造一个真实的 AliyunCertificate 当 owner——ownerRef 必须指向存在的对象，
	// 否则 GC 会立刻把 Secret 删掉，测出来的就不是 cert-manager 的行为。
	//
	// IssuerRef 必须显式写死成本 namespace 里那个 SelfSigned Issuer。留空会回落到
	// operator 的 --default-issuer-*（本仓库默认 letsencrypt-prod / ClusterIssuer），
	// 于是在一个装了本 operator 的集群上，这个对象会让 cert-manager 对一个 .invalid
	// 域名开真实的 ACME order——ClusterIssuer、ACME、LE 配额，Ruling P3-R16 的三条
	// 「绝不」一次踩满。
	issuerRef := selfSignedIssuerRef // 取副本，不把包级变量的地址交出去
	owner := &certsv1alpha1.AliyunCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-owner", Namespace: ns},
		Spec: certsv1alpha1.AliyunCertificateSpec{
			CertificateTemplate: certsv1alpha1.CertificateTemplate{
				DNSNames:  []string{san1},
				IssuerRef: &issuerRef,
			},
			Aliyun: certsv1alpha1.AliyunSpec{
				CredentialsRef: certsv1alpha1.LocalSecretReference{Name: "unused"},
				Region:         "cn-hangzhou",
			},
		},
	}
	if err := c.Create(ctx, owner); err != nil {
		p.skip(t, "创建 owner AliyunCertificate 失败："+errBrief(err))
	}

	waitSecret(t, c, p, ns, secretName)
	// 重新 Get 再 Update：cert-manager 也在并发写这个 Secret，拿一份陈旧的副本改
	// 必然 Conflict。RetryOnConflict 的正确用法是每次重试都重新读。
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &corev1.Secret{}
		if err := c.Get(ctx, secretKey, cur); err != nil {
			return err
		}
		cur.OwnerReferences = append(cur.OwnerReferences, metav1.OwnerReference{
			APIVersion: certsv1alpha1.GroupVersion.String(),
			Kind:       "AliyunCertificate",
			Name:       owner.Name,
			UID:        owner.UID,
		})
		return c.Update(ctx, cur)
	})
	if err != nil {
		p.skip(t, "给 Secret 加 ownerRef 失败："+errBrief(err))
	}

	// 改 dnsNames 触发重签（spec §2.3：改 dnsNames 会重签）。
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cert := &cmapi.Certificate{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: certName}, cert); err != nil {
			return err
		}
		cert.Spec.DNSNames = []string{san1, san2}
		return c.Update(ctx, cert)
	})
	if err != nil {
		p.skip(t, "改 Certificate 的 dnsNames 失败："+errBrief(err))
	}

	// reissued 与 kept 必须分开跟踪。只看 kept 的话，「三分钟内压根没重签」会和
	// 「重签了、ownerRef 被抹掉」得出同一个 false，于是一次超时会静默写下
	// 「不保留」——而这条结论决定 README 要不要保留 operator 的 secrets: delete。
	// 一个没有任何运行时证据支撑的保守结论比报错更坏，因为没人会去质疑它。
	deadline := time.Now().Add(3 * time.Minute)
	reissued, kept := false, false
	lastErr := ""
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		cur := &corev1.Secret{}
		if err := c.Get(ctx, secretKey, cur); err != nil {
			lastErr = errBrief(err)
			continue
		}
		if !hasSAN(t, cur.Data["tls.crt"], san2) {
			continue // 还没重签完
		}
		reissued = true
		for _, or := range cur.OwnerReferences {
			if or.Kind == "AliyunCertificate" && or.Name == owner.Name {
				kept = true
			}
		}
		break
	}
	if !reissued {
		why := "3 分钟内没有观测到重签（新 SAN " + san2 + " 始终没出现在 tls.crt 里），" +
			"无法区分「ownerRef 被 SSA 抹掉」与「什么都没发生」"
		if lastErr != "" {
			why += "；最后一次读 Secret 失败：" + lastErr
		}
		p.skip(t, why)
	}
	result := "不保留：重签后 ownerRef 被 SSA 抹掉"
	if kept {
		result = "保留：重签后 ownerRef 仍在"
	}
	p.record(t, result, certManagerVersion(t, c)+"，SelfSigned Issuer，改 dnsNames 触发重签并已观测到新 SAN 生效")
}

// TestSecretReplacementBumpsRevision（§12.3 #7）：operator 不缓存 Secret，靠 1h
// resync 兜底。如果 cert-manager 会因为 Secret 被换掉而重签并 bump revision，
// 盲区就小得多——watch Certificate 就能第一时间知道。
func TestSecretReplacementBumpsRevision(t *testing.T) {
	p := probeRevision
	c := clusterClient(t, p, kindCertificate, kindIssuer)
	ctx := context.Background()
	ns, certName, secretName := setupIssuedCertificate(t, c, p, "probe7.integration.invalid")
	certKey := client.ObjectKey{Namespace: ns, Name: certName}
	secretKey := client.ObjectKey{Namespace: ns, Name: secretName}

	// 基线必须等 status.revision 真的出现之后再取，理由见 waitRevision。
	beforeRev := waitRevision(t, c, p, ns, certName)

	// 换成另一张「同样合法、SAN 也满足 spec」的证书，但不是 cert-manager 签的那张。
	other := selfSignedFor(t, "probe7.integration.invalid")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &corev1.Secret{}
		if err := c.Get(ctx, secretKey, cur); err != nil {
			return err
		}
		cur.Data["tls.crt"] = other.certPEM
		cur.Data["tls.key"] = other.keyPEM
		return c.Update(ctx, cur)
	})
	if err != nil {
		p.skip(t, "替换 Secret 内容失败："+errBrief(err))
	}

	// sawCert 区分「读到了 Certificate、revision 就是没动」与「整整三分钟一次都没读到」。
	// 后者是基础设施故障，把它写成「cert-manager 不重签」等于伪造结论。
	deadline := time.Now().Add(3 * time.Minute)
	sawCert, bumped := false, false
	lastErr := ""
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		cur := &cmapi.Certificate{}
		if err := c.Get(ctx, certKey, cur); err != nil {
			lastErr = errBrief(err)
			continue
		}
		sawCert = true
		if revisionOf(cur) > beforeRev {
			bumped = true
			break
		}
	}
	if !bumped && !sawCert {
		p.skip(t, "3 分钟内一次都没读到 Certificate，无法判断 revision 是否变化；最后一次读取失败："+lastErr)
	}
	result := "不重签：3 分钟内 revision 未变，盲区只能靠 resync 兜住"
	if bumped {
		result = "重签并 bump revision：watch Certificate 即可发现"
	}
	p.record(t, result, "替换前 revision="+itoa(int64(beforeRev))+"（已确认 status.revision 已就绪后才取基线）")
}

// TestCertManagerSecretAnnotations（§12.3 #11）：SecretNameConflict 的判定要靠这些
// 注解认出「这个 Secret 是不是我们那张 Certificate 的产物」。
func TestCertManagerSecretAnnotations(t *testing.T) {
	p := probeAnnotations
	c := clusterClient(t, p, kindCertificate, kindIssuer)
	ns, _, secretName := setupIssuedCertificate(t, c, p, "probe11.integration.invalid")
	s := waitSecret(t, c, p, ns, secretName)

	// 用 cmapi 的常量而不是字面量：cert-manager 哪天改了 key，这里编译期就红，
	// 而不是报一个「注解缺失」的假结论。
	want := []string{
		cmapi.CertificateNameKey,
		cmapi.IssuerNameAnnotationKey,
		cmapi.IssuerKindAnnotationKey,
		cmapi.IssuerGroupAnnotationKey,
	}
	var missing []string
	for _, k := range want {
		if s.Annotations[k] == "" {
			missing = append(missing, k)
		}
	}
	result := "四个注解全部存在，可作为 SecretNameConflict 的判定依据"
	detail := cmapi.CertificateNameKey + "=" + s.Annotations[cmapi.CertificateNameKey]
	if len(missing) > 0 {
		result = "缺失：" + joinComma(missing)
		t.Errorf("cert-manager 没打这些注解: %v", missing)
	}
	p.record(t, result, detail)
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
