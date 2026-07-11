package config

import (
	"context"
	"log"

	"github.com/sagernet/sing/common/logger"
)

// realmLogger 是注入给 egress.Config.Logger 的最小 ContextLogger，把 sing-quic
// realm 内部（STUN 发现 / Register / Heartbeat / event-stream）的日志转到 agent 的
// 标准 log（→ systemd journal）。此前 generator 不设 Logger → sing-quic 用 NOP →
// 会合面注册全过程不可观测（排障黑洞）。带 [realm] 前缀便于 journalctl 过滤。
var _ logger.ContextLogger = realmLogger{}

type realmLogger struct{}

func (realmLogger) Trace(args ...any) { log.Print(append([]any{"[realm][trace] "}, args...)...) }
func (realmLogger) Debug(args ...any) { log.Print(append([]any{"[realm][debug] "}, args...)...) }
func (realmLogger) Info(args ...any)  { log.Print(append([]any{"[realm][info] "}, args...)...) }
func (realmLogger) Warn(args ...any)  { log.Print(append([]any{"[realm][warn] "}, args...)...) }
func (realmLogger) Error(args ...any) { log.Print(append([]any{"[realm][error] "}, args...)...) }
func (realmLogger) Fatal(args ...any) { log.Print(append([]any{"[realm][fatal] "}, args...)...) }
func (realmLogger) Panic(args ...any) { log.Print(append([]any{"[realm][panic] "}, args...)...) }

func (l realmLogger) TraceContext(_ context.Context, a ...any) { l.Trace(a...) }
func (l realmLogger) DebugContext(_ context.Context, a ...any) { l.Debug(a...) }
func (l realmLogger) InfoContext(_ context.Context, a ...any)  { l.Info(a...) }
func (l realmLogger) WarnContext(_ context.Context, a ...any)  { l.Warn(a...) }
func (l realmLogger) ErrorContext(_ context.Context, a ...any) { l.Error(a...) }
func (l realmLogger) FatalContext(_ context.Context, a ...any) { l.Fatal(a...) }
func (l realmLogger) PanicContext(_ context.Context, a ...any) { l.Panic(a...) }
