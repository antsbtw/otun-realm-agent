package realm

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvaluate_DecisionMatrix(t *testing.T) {
	r := NewRegistrar()

	// 用一个肯定不可达的地址（保留文档地址段 + 不存在端口）模拟 L3 不可达。
	// 192.0.2.0/24 是 TEST-NET-1，路由黑洞。
	unreachable := "http://192.0.2.1:9/realm"

	d := r.Evaluate(unreachable, true, false)
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

// ★obs 探测器 bug 修复回归：自签 TLS 会合面(=de-01 的 otun-s-test https://ip:9443)。
//   insecure=false(验证证书)→ 假报不可达；insecure=true(rendezvous_insecure)→ 正确报可达。
//   这是 de-01 类出口 l3_reachable 恒假报 false 的根因，删 insecure 分支→本测试红。
func TestProbeL3_SelfSignedRendezvous(t *testing.T) {
	// httptest.NewTLSServer 用自签证书(同 otun-s-test 的 CN=otun-s-test 自签)。
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 会合面对 / 返 404，任何 HTTP 响应=端点在线。
	}))
	defer srv.Close()

	r := NewRegistrar()

	// insecure=false：验证自签证书失败 → 假报不可达（这正是修复前 de-01 的症状）。
	if got := r.ProbeL3(srv.URL, false); got.Reachable {
		t.Errorf("verify-cert probe of self-signed server should be unreachable (cert fail), got reachable")
	}
	// insecure=true：跳过验证 → 正确报可达（修复后 de-01 应如此）。
	if got := r.ProbeL3(srv.URL, true); !got.Reachable {
		t.Errorf("insecure probe of self-signed server should be reachable, got unreachable")
	}
}

// 确认 insecure 客户端确实配了 skip-verify（防将来误删 Transport）。
func TestInsecureClientSkipsVerify(t *testing.T) {
	r := NewRegistrar()
	tr, ok := r.httpClientInsecure.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("httpClientInsecure must have InsecureSkipVerify=true")
	}
	// 正式客户端不得 skip（自签会合面之外仍验证）。
	if r.httpClient.Transport != nil {
		if tr2, ok := r.httpClient.Transport.(*http.Transport); ok && tr2.TLSClientConfig != nil && tr2.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("httpClient (verify) must NOT skip verify")
		}
	}
	_ = tls.Config{}
}

// statusServer 模拟 otun-s 的 GET /v1/{slot}/status 端点（阶段 1a）。
// perSlot: slot → registered；不在表里的 slot 返回 404（模拟老会合面/未知 slot）。
// wantToken 非空时校验 Bearer，不符返 401。
func statusServer(t *testing.T, wantToken string, perSlot map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		// 路径形如 /v1/{slot}/status
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 3 || parts[0] != "v1" || parts[2] != "status" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not_found"}`))
			return
		}
		registered, known := perSlot[parts[1]]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not_found","message":"unknown path"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if registered {
			w.Write([]byte(`{"registered":true}`))
		} else {
			w.Write([]byte(`{"registered":false}`))
		}
	}))
}

func TestProbeRegistered_AllRegistered(t *testing.T) {
	srv := statusServer(t, "tok", map[string]bool{"base-hy2": true, "base-reality": true})
	defer srv.Close()
	r := NewRegistrar()
	ok, unreg := r.ProbeRegistered(srv.URL, "tok", []string{"base-hy2", "base-reality"}, false)
	if !ok || len(unreg) != 0 {
		t.Fatalf("want all registered, got ok=%v unreg=%v", ok, unreg)
	}
}

func TestProbeRegistered_OneSlotLost(t *testing.T) {
	srv := statusServer(t, "tok", map[string]bool{"base-hy2": true, "base-tuic": false})
	defer srv.Close()
	r := NewRegistrar()
	ok, unreg := r.ProbeRegistered(srv.URL, "tok", []string{"base-hy2", "base-tuic"}, false)
	if ok {
		t.Fatal("one slot conclusively unregistered → registeredSelf must be false")
	}
	if len(unreg) != 1 || unreg[0] != "base-tuic" {
		t.Fatalf("unreg=%v want [base-tuic]", unreg)
	}
}

// ★fail-safe 核心：老会合面（无 /status 端点，404）、鉴权失败、网络错都不得判未注册
// ——否则 agent 先于会合面升级部署会陷入 rebuild 风暴断在线用户。
func TestProbeRegistered_FailSafeUnknown(t *testing.T) {
	r := NewRegistrar()

	// 老会合面：所有 slot 404。
	oldSrv := statusServer(t, "", map[string]bool{})
	ok, unreg := r.ProbeRegistered(oldSrv.URL, "tok", []string{"base-hy2"}, false)
	oldSrv.Close()
	if !ok || len(unreg) != 0 {
		t.Fatalf("404 (old rendezvous) must be fail-safe registered, got ok=%v unreg=%v", ok, unreg)
	}

	// 错 token → 401：未知，不判未注册。
	authSrv := statusServer(t, "right-token", map[string]bool{"base-hy2": false})
	ok, _ = r.ProbeRegistered(authSrv.URL, "wrong-token", []string{"base-hy2"}, false)
	authSrv.Close()
	if !ok {
		t.Fatal("401 must be fail-safe registered")
	}

	// 网络错（黑洞地址）：未知，不判未注册。
	ok, _ = r.ProbeRegistered("http://192.0.2.1:9", "tok", []string{"base-hy2"}, false)
	if !ok {
		t.Fatal("network error must be fail-safe registered")
	}

	// 200 但畸形 JSON：未知，不判未注册。
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`not json`))
	}))
	ok, _ = r.ProbeRegistered(badSrv.URL, "tok", []string{"base-hy2"}, false)
	badSrv.Close()
	if !ok {
		t.Fatal("malformed body must be fail-safe registered")
	}

	// 空入参：无从探测，按已注册。
	if ok, _ := r.ProbeRegistered("", "tok", []string{"x"}, false); !ok {
		t.Fatal("empty serverURL must be fail-safe registered")
	}
	if ok, _ := r.ProbeRegistered("http://127.0.0.1:1", "tok", nil, false); !ok {
		t.Fatal("no slots must be fail-safe registered")
	}
}

// 自签会合面（rendezvous_insecure=true）：status 探测同样必须跳过证书验证，
// 否则对自签面恒探不到（等同 ProbeL3 的老 bug 复刻到新端点）。
func TestProbeRegistered_SelfSignedRendezvous(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"registered":false}`))
	}))
	defer srv.Close()
	r := NewRegistrar()

	// insecure=false：证书验证失败 → 未知 → fail-safe 已注册。
	if ok, _ := r.ProbeRegistered(srv.URL, "tok", []string{"base-hy2"}, false); !ok {
		t.Fatal("cert failure must be fail-safe registered")
	}
	// insecure=true：探测成功 → 确凿未注册。
	if ok, _ := r.ProbeRegistered(srv.URL, "tok", []string{"base-hy2"}, true); ok {
		t.Fatal("insecure probe must reach self-signed server and report unregistered")
	}
}

func TestConfirmUnregistered_Streak(t *testing.T) {
	r := NewRegistrar()

	// 第一次未注册：不触发（等确认）。
	if r.ConfirmUnregistered(true) {
		t.Fatal("first unregistered tick must not trigger rebuild")
	}
	// 连续第二次：触发。
	if !r.ConfirmUnregistered(true) {
		t.Fatal("second consecutive unregistered tick must trigger rebuild")
	}
	// 触发后清零：再一次未注册又要重新确认。
	if r.ConfirmUnregistered(true) {
		t.Fatal("streak must reset after firing")
	}
	// 中途恢复注册 → 清零。
	if r.ConfirmUnregistered(false) {
		t.Fatal("registered tick must not trigger")
	}
	if r.ConfirmUnregistered(true) {
		t.Fatal("streak must restart after a healthy tick")
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
