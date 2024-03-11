// SPDX-License-Identifier: GPL-2.0-or-later

package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"nvr/pkg/ffmpeg"
	"nvr/pkg/log"
	"nvr/pkg/storage"
	"nvr/pkg/video/customformat"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/hls"
	"nvr/pkg/video/mp4muxer"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Recorder creates and saves new recordings.
type Recorder struct {
	Config Config

	events     *storage.Events
	eventsLock sync.Mutex
	eventChan  chan storage.Event

	logf       logFunc
	runSession runRecordingFunc
	NewProcess ffmpeg.NewProcessFunc

	input  *InputProcess
	Env    storage.ConfigEnv
	Logger log.ILogger
	wg     *sync.WaitGroup
	hooks  Hooks

	sleep   time.Duration
	prevSeg *hls.Segment
}

func newRecorder(m *Monitor) *Recorder {
	monitorID := m.Config.ID()
	logf := func(level log.Level, format string, a ...interface{}) {
		msg := fmt.Sprintf(format, a...)
		m.Logger.Log(log.Entry{
			Level:     level,
			Src:       "recorder",
			MonitorID: monitorID,
			Msg:       m.Env.CensorLog(msg),
		})
	}
	return &Recorder{
		Config: m.Config,

		events:     &storage.Events{},
		eventsLock: sync.Mutex{},
		eventChan:  make(chan storage.Event),

		logf:       logf,
		runSession: runRecording,
		NewProcess: ffmpeg.NewProcess,

		input:  m.mainInput,
		Env:    m.Env,
		Logger: m.Logger,
		wg:     &m.WG,
		hooks:  m.hooks,

		sleep: 3 * time.Second,
	}
}

func (r *Recorder) start(ctx context.Context) {
	defer r.wg.Done()

	var sessionCtx context.Context
	var cancelSession context.CancelFunc
	isRecording := false
	triggerTimer := &time.Timer{}
	onSessionExit := make(chan struct{})

	var timerEnd time.Time
	for {
		select {
		case <-ctx.Done():
			if cancelSession != nil {
				cancelSession()
			}
			if isRecording {
				// Wait for session to exit.
				<-onSessionExit
			}
			return

		case event := <-r.eventChan: // Incomming events.
			r.hooks.Event(r, &event)
			r.eventsLock.Lock()
			*r.events = append(*r.events, event)
			r.eventsLock.Unlock()

			end := event.Time.Add(event.RecDuration)
			if end.After(timerEnd) {
				timerEnd = end
			}

			if isRecording {
				r.logf(log.LevelDebug, "new event, already recording, updating timer")
				triggerTimer = time.NewTimer(time.Until(timerEnd))
				continue
			}

			r.logf(log.LevelDebug, "starting recording session")
			isRecording = true
			triggerTimer = time.NewTimer(time.Until(timerEnd))
			sessionCtx, cancelSession = context.WithCancel(ctx)
			go func() {
				r.runRecordingSession(sessionCtx)
				onSessionExit <- struct{}{}
			}()

		case <-triggerTimer.C:
			r.logf(log.LevelDebug, "timer reached end, canceling session")
			cancelSession()

		case <-onSessionExit:
			// Recording was canceled and stopped.
			isRecording = false
			continue
		}
	}
}

func (r *Recorder) runRecordingSession(ctx context.Context) {
	defer r.logf(log.LevelDebug, "session stopped")
	for {
		err := r.runSession(ctx, r)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				r.logf(log.LevelError, "recording crashed: %v", err)
			}
			select {
			case <-ctx.Done():
				// Session is canceled.
				return
			case <-time.After(r.sleep):
				r.logf(log.LevelDebug, "recovering after crash")
			}
		} else {
			r.logf(log.LevelInfo, "recording finished")
			if ctx.Err() != nil {
				// Session is canceled.
				return
			}
			// Recoding reached videoLength and exited normally. The timer
			// is still active, so continue the loop and start another one.
			continue
		}
	}
}

type runRecordingFunc func(context.Context, *Recorder) error

func runRecording(ctx context.Context, r *Recorder) error {
	timestampOffsetInt, err := strconv.Atoi(r.Config.TimestampOffset())
	if err != nil {
		return fmt.Errorf("parse timestamp offset %w", err)
	}

	muxer, err := r.input.HLSMuxer(ctx)
	if err != nil {
		return fmt.Errorf("get muxer: %w", err)
	}

	firstSegment, err := muxer.NextSegment(r.prevSeg)
	if err != nil {
		return fmt.Errorf("first segment: %w", err)
	}

	offset := 0 + time.Duration(timestampOffsetInt)*time.Millisecond
	startTime := firstSegment.StartTime.Add(-offset)

	monitorID := r.Config.ID()
	fileDir := filepath.Join(
		r.Env.RecordingsDir(),
		startTime.Format("2006/01/02/")+monitorID,
	)
	filePath := filepath.Join(
		fileDir,
		startTime.Format("2006-01-02_15-04-05_")+monitorID,
	)
	basePath := filepath.Base(filePath)

	err = os.MkdirAll(fileDir, 0o755)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("make directory for video: %w", err)
	}

	videoLengthStr := r.Config.videoLength()
	videoLengthFloat, err := strconv.ParseFloat(videoLengthStr, 64)
	if err != nil {
		return fmt.Errorf("parse video length: %w", err)
	}
	videoLength := time.Duration(videoLengthFloat * float64(time.Minute))

	r.logf(log.LevelInfo, "starting recording: %v", basePath)

	videoTrack := muxer.VideoTrack()
	audioTrack := muxer.AudioTrack()
	go r.generateThumbnail(filePath, firstSegment, videoTrack)

	prevSeg, endTime, err := generateVideo(
		ctx, filePath, muxer.NextSegment, firstSegment, videoTrack, audioTrack, videoLength)
	if err != nil {
		return fmt.Errorf("write video: %w", err)
	}
	r.prevSeg = prevSeg
	r.logf(log.LevelInfo, "video generated: %v", basePath)

	go r.saveRecording(filePath, startTime, *endTime)

	return nil
}

// ErrSkippedSegment skipped segment.
var ErrSkippedSegment = errors.New("skipped segment")

type nextSegmentFunc func(*hls.Segment) (*hls.Segment, error)

func generateVideo( //nolint:funlen
	ctx context.Context,
	filePath string,
	nextSegment nextSegmentFunc,
	firstSegment *hls.Segment,
	videoTrack *gortsplib.TrackH264,
	audioTrack *gortsplib.TrackMPEG4Audio,
	maxDuration time.Duration,
) (*hls.Segment, *time.Time, error) {
	prevSeg := firstSegment
	startTime := firstSegment.StartTime
	stopTime := firstSegment.StartTime.Add(maxDuration)
	endTime := startTime

	metaPath := filePath + ".meta"
	mdatPath := filePath + ".mdat"

	meta, err := os.OpenFile(metaPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	defer meta.Close()

	mdat, err := os.OpenFile(mdatPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	defer mdat.Close()

	var audioConfig []byte
	if audioTrack != nil {
		audioConfig, err = audioTrack.Config.Marshal()
		if err != nil {
			return nil, nil, err
		}
	}

	header := customformat.Header{
		VideoSPS:    videoTrack.SPS,
		VideoPPS:    videoTrack.PPS,
		AudioConfig: audioConfig,
		StartTime:   startTime.UnixNano(),
	}

	w, err := customformat.NewWriter(meta, mdat, header)
	if err != nil {
		return nil, nil, err
	}

	writeSegment := func(seg *hls.Segment) error {
		if err := w.WriteSegment(seg); err != nil {
			return err
		}
		prevSeg = seg
		endTime = seg.StartTime.Add(seg.RenderedDuration)
		return nil
	}

	if err := writeSegment(firstSegment); err != nil {
		return nil, nil, err
	}

	for {
		if ctx.Err() != nil {
			return prevSeg, &endTime, nil
		}

		seg, err := nextSegment(prevSeg)
		if err != nil {
			return prevSeg, &endTime, nil
		}

		if seg.ID != prevSeg.ID+1 {
			return nil, nil, fmt.Errorf("%w: expected: %v got %v",
				ErrSkippedSegment, prevSeg.ID+1, seg.ID)
		}

		if err := writeSegment(seg); err != nil {
			return nil, nil, err
		}

		if seg.StartTime.After(stopTime) {
			return prevSeg, &endTime, nil
		}
	}
}

// The first h264 frame in firstSegment is wrapped in a mp4
// container and piped into FFmpeg and then converted to jpeg.
func (r *Recorder) generateThumbnail(
	filePath string,
	firstSegment *hls.Segment,
	videoTrack *gortsplib.TrackH264,
) {
	videoBuffer := &bytes.Buffer{}
	err := mp4muxer.GenerateThumbnailVideo(videoBuffer, firstSegment, videoTrack)
	if err != nil {
		r.logf(log.LevelError, "generate thumbnail video: %v", err)
		return
	}

	thumbPath := filePath + ".jpeg"
	args := "-n -threads 1 -loglevel " + r.Config.LogLevel() +
		" -i -" + // Input.
		" -frames:v 1 " + thumbPath // Output.

	r.logf(log.LevelInfo, "generating thumbnail: %v", thumbPath)

	r.hooks.RecSave(r, &args)

	cmd := exec.Command(r.Env.FFmpegBin, ffmpeg.ParseArgs(args)...)
	cmd.Stdin = videoBuffer

	ffLogLevel := log.FFmpegLevel(r.Config.LogLevel())
	logFunc := func(msg string) {
		r.logf(ffLogLevel, "thumbnail process: %v", msg)
	}
	process := r.NewProcess(cmd).
		StdoutLogger(logFunc).
		StderrLogger(logFunc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := process.Start(ctx); err != nil {
		r.logf(log.LevelError, "generate thumbnail, args: %v error: %v", args, err)
		return
	}
	r.logf(log.LevelDebug, "thumbnail generated: %v", filepath.Base(thumbPath))
}

func (r *Recorder) saveRecording(
	filePath string,
	startTime time.Time,
	endTime time.Time,
) {
	r.logf(log.LevelInfo, "saving recording: %v", filepath.Base(filePath))

	r.eventsLock.Lock()
	events := r.events.QueryAndPrune(startTime, endTime)
	r.eventsLock.Unlock()

	data := storage.RecordingData{
		Start:  startTime,
		End:    endTime,
		Events: events,
	}
	json, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		r.logf(log.LevelError, "marshal event data: %w", err)
		return
	}

	dataPath := filePath + ".json"
	if err := os.WriteFile(dataPath, json, 0o600); err != nil {
		r.logf(log.LevelError, "write event data: %v", err)
		return
	}

	go r.hooks.RecSaved(r, filePath, data)

	r.logf(log.LevelInfo, "recording saved: %v", filepath.Base(dataPath))
}

func (r *Recorder) sendEvent(ctx context.Context, event storage.Event) error {
	if err := event.Validate(); err != nil {
		return fmt.Errorf("invalid event: %w", err)
	}
	select {
	case <-ctx.Done():
		return context.Canceled
	case r.eventChan <- event:
		return nil
	}
}
