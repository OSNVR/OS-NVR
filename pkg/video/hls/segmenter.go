package hls

import (
	"bytes"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/gortsplib/pkg/h264"
	"time"
)

func partDurationIsCompatible(partDuration time.Duration, sampleDuration time.Duration) bool {
	if sampleDuration > partDuration {
		return false
	}

	f := (partDuration / sampleDuration)
	if (partDuration % sampleDuration) != 0 {
		f++
	}
	f *= sampleDuration

	return partDuration > ((f * 85) / 100)
}

func partDurationIsCompatibleWithAll(partDuration time.Duration, sampleDurations map[time.Duration]struct{}) bool {
	for sd := range sampleDurations {
		if !partDurationIsCompatible(partDuration, sd) {
			return false
		}
	}
	return true
}

func findCompatiblePartDuration(
	minPartDuration time.Duration,
	sampleDurations map[time.Duration]struct{},
) time.Duration {
	i := minPartDuration
	for ; i < 5*time.Second; i += 5 * time.Millisecond {
		if partDurationIsCompatibleWithAll(i, sampleDurations) {
			break
		}
	}
	return i
}

type segmenter struct {
	muxerID            uint16
	segmentDuration    time.Duration
	partDuration       time.Duration
	segmentMaxSize     uint64
	videoTrack         *gortsplib.TrackH264
	audioTrack         *gortsplib.TrackMPEG4Audio
	onSegmentFinalized func(*Segment)
	onPartFinalized    func(*MuxerPart)

	startDTS                       time.Duration
	muxerStartTime                 int64
	videoFirstRandomAccessReceived bool
	videoDTSExtractor              *h264.DTSExtractor
	lastVideoParams                [][]byte
	nextSegmentID                  uint64
	videoSPS                       []byte
	currentSegment                 *Segment
	nextPartID                     uint64
	nextVideoSample                *VideoSample
	nextAudioSample                *AudioSample
	firstSegmentFinalized          bool
	sampleDurations                map[time.Duration]struct{}
	adjustedPartDuration           time.Duration
}

func newSegmenter(
	muxerID uint16,
	muxerStartTime int64,
	segmentDuration time.Duration,
	partDuration time.Duration,
	segmentMaxSize uint64,
	videoTrack *gortsplib.TrackH264,
	audioTrack *gortsplib.TrackMPEG4Audio,
	onSegmentFinalized func(*Segment),
	onPartFinalized func(*MuxerPart),
) *segmenter {
	return &segmenter{
		muxerID:            muxerID,
		segmentDuration:    segmentDuration,
		partDuration:       partDuration,
		segmentMaxSize:     segmentMaxSize,
		videoTrack:         videoTrack,
		audioTrack:         audioTrack,
		onSegmentFinalized: onSegmentFinalized,
		onPartFinalized:    onPartFinalized,
		muxerStartTime:     muxerStartTime,
		nextSegmentID:      7, // Required by iOS.
		sampleDurations:    make(map[time.Duration]struct{}),
	}
}

func (m *segmenter) genSegmentID() uint64 {
	id := m.nextSegmentID
	m.nextSegmentID++
	return id
}

func (m *segmenter) genPartID() uint64 {
	id := m.nextPartID
	m.nextPartID++
	return id
}

// iPhone iOS fails if part durations are less than 85% of maximum part duration.
// find a part duration that is compatible with all received sample durations.
func (m *segmenter) adjustPartDuration(du time.Duration) {
	if m.firstSegmentFinalized {
		return
	}

	// Avoid a crash by skipping invalid durations.
	if du == 0 {
		return
	}

	if _, ok := m.sampleDurations[du]; !ok {
		m.sampleDurations[du] = struct{}{}
		m.adjustedPartDuration = findCompatiblePartDuration(
			m.partDuration,
			m.sampleDurations,
		)
	}
}

func (m *segmenter) writeH264(ntp time.Time, pts time.Duration, au [][]byte) error {
	randomAccessPresent := false
	nonIDRPresent := false

	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case h264.NALUTypeIDR:
			randomAccessPresent = true

		case h264.NALUTypeNonIDR:
			nonIDRPresent = true
		}
	}

	if !randomAccessPresent && !nonIDRPresent {
		return nil
	}

	return m.writeH264Entry(ntp, pts, au, randomAccessPresent)
}

func (m *segmenter) writeH264Entry( //nolint:funlen
	ntp time.Time,
	pts time.Duration,
	au [][]byte,
	randomAccessPresent bool,
) error {
	var dts time.Duration

	if !m.videoFirstRandomAccessReceived {
		// skip sample silently until we find one with an IDR
		if !randomAccessPresent {
			return nil
		}

		m.videoFirstRandomAccessReceived = true
		m.videoDTSExtractor = h264.NewDTSExtractor()
		m.videoSPS = m.videoTrack.SPS

		var err error
		dts, err = m.videoDTSExtractor.Extract(au, dts)
		if err != nil {
			return err
		}

		m.startDTS = dts
		dts = 0
		pts -= m.startDTS
	} else {
		var err error
		dts, err = m.videoDTSExtractor.Extract(au, pts)
		if err != nil {
			return err
		}

		pts -= m.startDTS
		dts -= m.startDTS
	}

	avcc := h264.AVCCMarshal(au)

	sample := &VideoSample{
		PTS:        m.muxerStartTime + int64(pts),
		DTS:        m.muxerStartTime + int64(dts),
		AVCC:       avcc,
		IdrPresent: randomAccessPresent,
	}

	// put samples into a queue in order to
	// - compute sample duration
	// - check if next sample is IDR
	sample, m.nextVideoSample = m.nextVideoSample, sample
	if sample == nil {
		return nil
	}

	sample.Duration = func() time.Duration {
		if m.nextVideoSample.DTS-sample.DTS < 0 {
			return 0
		}
		return time.Duration(m.nextVideoSample.DTS - sample.DTS)
	}()

	if m.currentSegment == nil {
		// create first segment
		m.currentSegment = newSegment(
			m.genSegmentID(),
			m.muxerID,
			ntp,
			time.Duration(sample.DTS-m.muxerStartTime),
			m.muxerStartTime,
			m.segmentMaxSize,
			m.audioTrack,
			m.genPartID,
			m.onPartFinalized,
		)
	}

	m.adjustPartDuration(sample.Duration)

	err := m.currentSegment.writeH264(sample, m.adjustedPartDuration)
	if err != nil {
		return err
	}

	// switch segment
	if randomAccessPresent {
		videoParams := extractVideoParams(m.videoTrack)
		paramsChanged := !videoParamsEqual(m.lastVideoParams, videoParams)

		if (time.Duration(m.nextVideoSample.DTS)-m.currentSegment.startDTS) >= m.segmentDuration ||
			paramsChanged {
			err := m.currentSegment.finalize(m.nextVideoSample)
			if err != nil {
				return err
			}
			m.onSegmentFinalized(m.currentSegment)

			m.firstSegmentFinalized = true

			m.currentSegment = newSegment(
				m.genSegmentID(),
				m.muxerID,
				ntp,
				time.Duration(sample.DTS-m.muxerStartTime),
				m.muxerStartTime,
				m.segmentMaxSize,
				m.audioTrack,
				m.genPartID,
				m.onPartFinalized,
			)

			if paramsChanged {
				m.lastVideoParams = videoParams
				m.firstSegmentFinalized = false

				// reset adjusted part duration
				m.sampleDurations = make(map[time.Duration]struct{})
			}
		}
	}

	return nil
}

func extractVideoParams(track *gortsplib.TrackH264) [][]byte {
	params := make([][]byte, 2)
	params[0] = track.SafeSPS()
	params[1] = track.SafePPS()
	return params
}

func videoParamsEqual(p1 [][]byte, p2 [][]byte) bool {
	if len(p1) != len(p2) {
		return true
	}

	for i, p := range p1 {
		if !bytes.Equal(p2[i], p) {
			return false
		}
	}
	return true
}

func (m *segmenter) writeAAC(pts time.Duration, au []byte) error {
	return m.writeAACEntry(&AudioSample{
		PTS: int64(pts),
		AU:  au,
	})
}

func (m *segmenter) writeAACEntry(sample *AudioSample) error {
	// wait for the video track
	if !m.videoFirstRandomAccessReceived {
		return nil
	}

	sample.PTS -= int64(m.startDTS)
	sample.PTS += m.muxerStartTime

	// put samples into a queue in order to
	// allow to compute the sample duration
	sample, m.nextAudioSample = m.nextAudioSample, sample
	if sample == nil {
		return nil
	}

	sample.NextPTS = m.nextAudioSample.PTS

	// wait for the video track
	if m.currentSegment == nil {
		return nil
	}

	err := m.currentSegment.writeAAC(sample)
	if err != nil {
		return err
	}

	return nil
}
