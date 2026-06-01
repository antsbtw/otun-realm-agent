package realm

import "testing"

func TestEvaluate_DecisionMatrix(t *testing.T) {
	r := NewRegistrar()

	// 用一个肯定不可达的地址（保留文档地址段 + 不存在端口）模拟 L3 不可达。
	// 192.0.2.0/24 是 TEST-NET-1，路由黑洞。
	unreachable := "http://192.0.2.1:9/realm"

	d := r.Evaluate(unreachable, true)
	if d.Reachable {
		t.Skip("环境意外可达 TEST-NET-1，跳过")
	}
	// L3 不可达 → 绝不 reload（reload 也连不上）。
	if d.NeedReload {
		t.Errorf("L3 不可达时不应 reload, got reason=%s", d.Reason)
	}
	if d.Reason != "l3_unreachable_wait" {
		t.Errorf("unexpected reason: %s", d.Reason)
	}
}

func TestHostPortFromURL(t *testing.T) {
	cases := map[string]string{
		"https://situstechnologies.com/realm": "situstechnologies.com:443",
		"http://1.2.3.4:8080/x":               "1.2.3.4:8080",
		"situstechnologies.com":               "situstechnologies.com:443",
	}
	for in, want := range cases {
		if got := hostPortFromURL(in); got != want {
			t.Errorf("hostPortFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
