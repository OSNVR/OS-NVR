// SPDX-License-Identifier: GPL-2.0-or-later

package monitor

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/pkg/ffmpeg"
	"nvr/pkg/ffmpeg/ffmock"
	"nvr/pkg/log"
	"nvr/pkg/storage"
	"nvr/pkg/video"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/hls"

	"github.com/stretchr/testify/require"
)

func newTestRecorder(t *testing.T) *Recorder {
	t.Helper()
	tempDir := t.TempDir()
	t.Cleanup(func() {
		os.Remove(tempDir)
	})

	logf := func(level log.Level, format string, a ...interface{}) {}
	return &Recorder{
		Config: NewConfig(RawConfig{
			"timestampOffset": "0",
			"videoLength":     "0.0003",
		}),

		events:     &storage.Events{},
		eventsLock: sync.Mutex{},
		eventChan:  make(chan storage.Event),

		logf:       logf,
		runSession: runRecording,
		NewProcess: ffmock.NewProcess,

		input: &InputProcess{
			isSubInput: false,

			serverPath: video.ServerPath{
				HlsAddress: "hls.m3u8",
				HLSMuxer: newMockMuxerFunc(
					&mockMuxer{videoTrack: &gortsplib.TrackH264{SPS: []byte{0, 0, 0}}},
				),
			},

			logf: logf,

			runInputProcess: stubRunInputProcess,
			newProcess:      ffmock.NewProcess,
		},
		wg: &sync.WaitGroup{},
		Env: storage.ConfigEnv{
			TempDir:    tempDir,
			StorageDir: tempDir,
		},
		hooks: stubHooks(),
	}
}

type mockMuxer struct {
	videoTrack  *gortsplib.TrackH264
	audioTrack  *gortsplib.TrackMPEG4Audio
	getMuxerErr error
	segCount    int
}

func newMockMuxerFunc(muxer *mockMuxer) func(context.Context) (video.IHLSMuxer, error) {
	return func(ctx context.Context) (video.IHLSMuxer, error) {
		return muxer, muxer.getMuxerErr
	}
}

func (m *mockMuxer) VideoTrack() *gortsplib.TrackH264 {
	return m.videoTrack
}

func (m *mockMuxer) AudioTrack() *gortsplib.TrackMPEG4Audio {
	return m.audioTrack
}

func (m *mockMuxer) NextSegment(_ *hls.Segment) (*hls.Segment, error) {
	seg := &hls.Segment{
		ID:        uint64(m.segCount),
		StartTime: time.Unix(1*int64(m.segCount), 0),
	}
	m.segCount++
	return seg, nil
}

func (m *mockMuxer) WaitForSegFinalized() {}

func TestStartRecorder(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		onRunRecording := make(chan struct{})
		onCanceled := make(chan struct{})
		mockRunRecording := func(ctx context.Context, _ *Recorder) error {
			close(onRunRecording)
			<-ctx.Done()
			close(onCanceled)
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.sleep = 1 * time.Hour
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		err := r.sendEvent(ctx, storage.Event{
			Time:        time.Now().Add(time.Duration(-1) * time.Hour),
			RecDuration: 1,
		})
		require.NoError(t, err)

		<-onRunRecording
		<-onCanceled
	})
	t.Run("timeoutUpdate", func(t *testing.T) {
		onRunRecording := make(chan struct{})
		mockRunRecording := func(ctx context.Context, _ *Recorder) error {
			close(onRunRecording)
			<-ctx.Done()
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		now := time.Now()
		r.eventChan <- storage.Event{Time: now, RecDuration: 20 * time.Millisecond}
		r.eventChan <- storage.Event{Time: now, RecDuration: 60 * time.Millisecond}

		<-onRunRecording
	})
	t.Run("recordingCheck", func(t *testing.T) {
		onRunRecording := make(chan struct{})
		mockRunRecording := func(ctx context.Context, _ *Recorder) error {
			close(onRunRecording)
			<-ctx.Done()
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		now := time.Now()
		r.eventChan <- storage.Event{Time: now, RecDuration: 10 * time.Millisecond}
		r.eventChan <- storage.Event{Time: now, RecDuration: 11 * time.Millisecond}
		r.eventChan <- storage.Event{Time: now, RecDuration: 0 * time.Millisecond}

		<-onRunRecording
	})
	// Only update timeout if new time is after current time.
	t.Run("updateTimeout", func(t *testing.T) {
		onCancel := make(chan struct{})
		mockRunRecording := func(ctx context.Context, _ *Recorder) error {
			<-ctx.Done()
			close(onCancel)
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		now := time.Now()
		r.eventChan <- storage.Event{Time: now, RecDuration: 30 * time.Millisecond}
		r.eventChan <- storage.Event{Time: now, RecDuration: 1 * time.Millisecond}

		select {
		case <-time.After(15 * time.Millisecond):
		case <-onCancel:
			t.Fatal("the second trigger reset the timeout")
		}
	})
	t.Run("normalExit", func(t *testing.T) {
		onRunRecording := make(chan struct{})
		exitProcess := make(chan error)
		mockRunRecording := func(ctx context.Context, _ *Recorder) error {
			onRunRecording <- struct{}{}
			return <-exitProcess
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.sleep = 1 * time.Hour
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		now := time.Now()
		r.eventChan <- storage.Event{Time: now, RecDuration: 1 * time.Hour}

		<-onRunRecording
		exitProcess <- nil
		<-onRunRecording
		exitProcess <- nil
		<-onRunRecording
		close(onRunRecording)
		exitProcess <- ffmock.ErrMock
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		r := newTestRecorder(t)
		r.wg.Add(1)
		r.start(ctx)
	})
	t.Run("canceled2", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockRunRecording := func(context.Context, *Recorder) error {
			cancel()
			return nil
		}

		r := newTestRecorder(t)
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		now := time.Now()
		r.eventChan <- storage.Event{Time: now, RecDuration: 1 * time.Hour}
	})
	t.Run("canceledRecording", func(t *testing.T) {
		onCancel := make(chan struct{})
		mockRunRecording := func(ctx context.Context, _ *Recorder) error {
			<-ctx.Done()
			close(onCancel)
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		now := time.Now()
		r.eventChan <- storage.Event{Time: now, RecDuration: 0}
		<-onCancel
	})
	t.Run("crashAndRestart", func(t *testing.T) {
		onRunRecording := make(chan struct{})
		mockRunRecording := func(ctx context.Context, _ *Recorder) error {
			onRunRecording <- struct{}{}
			return errors.New("mock")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.wg.Add(1)
		r.runSession = mockRunRecording
		go r.start(ctx)

		now := time.Now()
		r.eventChan <- storage.Event{Time: now, RecDuration: 1 * time.Hour}
		<-onRunRecording
		<-onRunRecording
		<-onRunRecording
	})
}

func createTempDir(t *testing.T, r *Recorder) {
}

func TestRunRecording(t *testing.T) {
	t.Run("saveRecordingAsync", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := newTestRecorder(t)
		r.NewProcess = ffmock.NewProcessNil
		r.hooks.RecSave = func(*Recorder, *string) {
			<-ctx.Done()
		}
		err := runRecording(ctx, r)
		require.NoError(t, err)
	})
	t.Run("crashed", func(t *testing.T) {
		r := newTestRecorder(t)
		r.Env.StorageDir = "/dev/null"

		err := runRecording(context.Background(), r)
		require.Error(t, err)
	})
	t.Run("mkdirErr", func(t *testing.T) {
		r := newTestRecorder(t)
		r.Env.StorageDir = "/dev/null"

		err := runRecording(context.Background(), r)
		require.Error(t, err)
	})
	t.Run("genArgsErr", func(t *testing.T) {
		r := newTestRecorder(t)
		r.Config.v["videoLength"] = ""

		err := runRecording(context.Background(), r)
		require.ErrorIs(t, err, strconv.ErrSyntax)
	})
	t.Run("parseOffsetErr", func(t *testing.T) {
		r := newTestRecorder(t)
		r.Config.v["timestampOffset"] = ""

		err := runRecording(context.Background(), r)
		require.ErrorIs(t, err, strconv.ErrSyntax)
	})
}

func TestWriteThumbnail(t *testing.T) {
	/*t.Run("ok", func(t *testing.T) {
		r := newTestRecorder(t)

		logs := make(chan string)
		r.logf = func(level log.Level, format string, a ...interface{}) {
			logs <- fmt.Sprintf(format, a...)
		}

		segment := &hls.Segment{
			Parts: []*hls.MuxerPart{{
				VideoSamples: []*hls.VideoSample{{
					IdrPresent: true,
				}},
			}},
		}
		videoTrack := &gortsplib.TrackH264{SPS: []byte{103, 0, 0, 0, 172, 217, 0}}

		done := make(chan struct{})
		go func() {
			r.generateThumbnail(os.TempDir(), segment, videoTrack)
			close(done)
		}()

		require.Equal(t, "generating thumbnail:", (<-logs)[:21])
		require.Equal(t, "thumbnail generated: tmp.jpeg", (<-logs))
		<-done
	})*/
	/*t.Run("processErr", func(t *testing.T) {
		r := newTestRecorder(t)
		r.NewProcess = ffmock.NewProcessErr

		logs := make(chan string)
		r.logf = func(level log.Level, format string, a ...interface{}) {
			logs <- fmt.Sprintf(format, a...)
		}

		segment := &hls.Segment{
			Parts: []*hls.MuxerPart{{
				VideoSamples: []*hls.VideoSample{{
					IdrPresent: true,
				}},
			}},
		}
		videoTrack := &gortsplib.TrackH264{SPS: []byte{103, 0, 0, 0, 172, 217, 0}}

		done := make(chan struct{})
		go func() {
			r.generateThumbnail(os.TempDir(), segment, videoTrack)
			close(done)
		}()

		require.Equal(t, "generating thumbnail:", (<-logs)[:21])
		require.Equal(t, "error: mock", (<-logs)[78:])
		<-done
	})*/
}

func TestSaveRecording(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		r := newTestRecorder(t)
		r.events = &storage.Events{
			storage.Event{
				Time: time.Time{},
			},
			storage.Event{
				Time: time.Time{}.Add(2 * time.Minute),
				Detections: []storage.Detection{
					{
						Label: "10",
						Score: 9,
						Region: &storage.Region{
							Rect: &ffmpeg.Rect{1, 2, 3, 4},
							Polygon: &ffmpeg.Polygon{
								ffmpeg.Point{5, 6},
								ffmpeg.Point{7, 8},
							},
						},
					},
				},
				Duration: 11,
			},
			storage.Event{
				Time: time.Time{}.Add(11 * time.Minute),
			},
		}

		start := time.Time{}.Add(1 * time.Minute)
		end := time.Time{}.Add(11 * time.Minute)
		tempdir := r.Env.TempDir
		filePath := tempdir + "file"

		r.saveRecording(filePath, start, end)

		b, err := os.ReadFile(filePath + ".json")
		require.NoError(t, err)

		actual := string(b)
		actual = strings.ReplaceAll(actual, " ", "")
		actual = strings.ReplaceAll(actual, "\n", "")

		expected := `{"start":"0001-01-01T00:01:00Z","end":"0001-01-01T00:11:00Z",` +
			`"events":[{"time":"0001-01-01T00:02:00Z","detections":` +
			`[{"label":"10","score":9,"region":{"rect":[1,2,3,4],` +
			`"polygon":[[5,6],[7,8]]}}],"duration":11}]}`

		require.Equal(t, actual, expected)
	})
}
