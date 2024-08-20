package watchdog

// Watchdog detects and restarts frozen processes.
// Freeze is detected by polling the output HLS manifest for file updates.

import (
	"context"
	"fmt"
	"nvr"
	"nvr/pkg/log"
	"nvr/pkg/monitor"
	"time"
)

func init() {
	nvr.RegisterMonitorInputProcessHook(onInputProcessStart)
	nvr.RegisterLogSource([]string{"watchdog"})
}

const defaultInterval = 15 * time.Second

func onInputProcessStart(ctx context.Context, i *monitor.InputProcess, _ *[]string) {
	monitorID := i.Config.ID()
	processName := i.ProcessName()

	logf := func(level log.Level, format string, a ...interface{}) {
		format = fmt.Sprintf("%v process: %s", processName, format)
		i.Logger.Log(log.Entry{
			Level:     level,
			Src:       "watchdog",
			MonitorID: monitorID,
			Msg:       fmt.Sprintf(format, a...),
		})
	}

	muxer := func(ctx context.Context) (muxer, error) {
		return i.HLSMuxer()
	}

	d := &watchdog{
		muxer:    muxer,
		interval: defaultInterval,
		onFreeze: i.Cancel,
		logf:     logf,
	}
	go d.start(ctx)
}

type muxer interface {
	WaitForSegFinalized()
}

type watchdog struct {
	muxer    func(context.Context) (muxer, error)
	interval time.Duration
	onFreeze func()
	logf     func(log.Level, string, ...interface{})
}

func (d *watchdog) start(ctx context.Context) {
	// Warmup.
	select {
	case <-time.After(d.interval):
	case <-ctx.Done():
		return
	}

	keepAlive := make(chan struct{})
	go func() {
		for {
			select {
			case <-time.After(1 * time.Second):
			case <-ctx.Done():
				return
			}

			muxer, err := d.muxer(ctx)
			if err != nil {
				continue
			}

			muxer.WaitForSegFinalized()
			select {
			case <-ctx.Done():
				return
			case keepAlive <- struct{}{}:
			}
		}
	}()

	for {
		select {
		case <-time.After(d.interval):
			d.logf(log.LevelError, "possible freeze detected, restarting..")
			d.onFreeze()
		case <-keepAlive:
		case <-ctx.Done():
			return
		}
	}
}
