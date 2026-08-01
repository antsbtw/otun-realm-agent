package obs

// punchtrace.go —— 接收端打洞埋点的开关与 schema（PUNCH_TRACE_DUAL_END_DESIGN §4）。
//
// 采集与组装都不在本仓：sing-quic 打洞引擎回调（PunchObserver）→ egress 库
// node/punchtrace 组装成 Record → 本仓只负责「要不要开」和「把 Record 装进
// obs 信封上报」。上报通道复用现成 Reporter（/obs/ingest，批量+缓存重传）。
//
// ★ 开关沿用 kernel transport/realm/probe_session.go 的模式与理由：
// 环境变量而非配置字段（配置会进下发契约，用户侧可能误开），默认关，
// 🔴 只认 "1"（不接受 true/yes/on，避免「设了 0 反而开了」类事故）。
// 关闭时 sink 为 nil → egress 库不建 Observer → 打洞热路径零观测开销。

import "os"

// SchemaPunchTraceEgress schema=punch_trace_egress（ops-collector ops_003 已建表，
// payload JSONB 原样落 obs_punch_trace_egress）。
const SchemaPunchTraceEgress = "punch_trace_egress"

// PunchTraceEnv 是打开接收端打洞埋点的环境变量名。值为 "1" 时开启。
// 与 kernel 发起端埋点同名——同一语义（"本进程要不要出打洞轨迹"）两端一致。
const PunchTraceEnv = "OTUN_PROBE_TRACE"

// PunchTraceEnabled 报告本进程是否开启接收端打洞埋点。
func PunchTraceEnabled() bool {
	return os.Getenv(PunchTraceEnv) == "1"
}
