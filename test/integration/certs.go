//go:build integration

package integration

import (
	"bytes"
	"encoding/pem"
	"testing"
)

// splitPEM 把一段可能含多个块的 PEM 拆成逐块编码的切片，顺序保持不变。
// 用于构造「只有 leaf」「leaf 与 intermediate 顺序颠倒」这类形状。
func splitPEM(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	rest := b
	for {
		blk, next := pem.Decode(rest)
		if blk == nil {
			break
		}
		out = append(out, pem.EncodeToMemory(blk))
		rest = next
	}
	if len(out) == 0 {
		t.Fatal("PEM 里一个块也没有")
	}
	return out
}

// joinPEM 按给定顺序拼接 PEM 块，块之间不留空行——这正是阿里云要求的形状
// （spec §2.2：证书之间不能有空行）。
func joinPEM(blocks ...[]byte) []byte {
	return bytes.Join(blocks, nil)
}
