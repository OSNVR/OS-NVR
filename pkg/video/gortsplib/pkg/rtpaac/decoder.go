package rtpaac

import (
	"encoding/binary"
	"errors"
	"fmt"
	"nvr/pkg/video/gortsplib/pkg/aac"
	"nvr/pkg/video/gortsplib/pkg/bits"
	"nvr/pkg/video/gortsplib/pkg/rtptimedec"
	"time"

	"github.com/pion/rtp"
)

// ErrMorePacketsNeeded is returned when more packets are needed.
var ErrMorePacketsNeeded = errors.New("need more packets")

// Decoder is a RTP/AAC decoder.
type Decoder struct {
	// sample rate of input packets.
	SampleRate int

	// The number of bits on which the AU-size field is encoded in the AU-header.
	SizeLength int

	// The number of bits on which the AU-Index is encoded in the first AU-header.
	IndexLength int

	// The number of bits on which the AU-Index-delta field is encoded in any non-first AU-header.
	IndexDeltaLength int

	timeDecoder       *rtptimedec.Decoder
	firstPacketParsed bool
	adtsMode          bool
	fragmentedMode    bool
	fragmentedParts   [][]byte
	fragmentedSize    int
}

// Init initializes the decoder.
func (d *Decoder) Init() {
	d.timeDecoder = rtptimedec.New(d.SampleRate)
}

// Errors.
var (
	ErrShortPayload        = errors.New("payload is too short")
	ErrAUinvalidLength     = errors.New("invalid AU-headers-length")
	ErrAUindexNotZero      = errors.New("AU-index different than zero is not supported")
	ErrAUindexDeltaNotZero = errors.New("AU-index-delta different than zero is not supported")
	ErrFragMultipleAU      = errors.New("a fragmented packet can only contain one AU")
	ErrADTSmultipleAU      = errors.New("multiple AUs in ADTS mode are not supported")
	ErrMultipleADTS        = errors.New("multiple ADTS packets are not supported")
)

// AUsizeToBigError .
type AUsizeToBigError struct {
	AUsize int
}

func (e AUsizeToBigError) Error() string {
	return fmt.Sprintf("AU size (%d) is too big (maximum is %d)", e.AUsize, aac.MaxAccessUnitSize)
}

// Decode decodes AUs from a RTP/AAC packet.
// It returns the AUs and the PTS of the first AU.
// The PTS of subsequent AUs can be calculated by adding time.Second*aac.SamplesPerAccessUnit/clockRate.
func (d *Decoder) Decode(pkt *rtp.Packet) ([][]byte, time.Duration, error) {
	if len(pkt.Payload) < 2 {
		d.fragmentedParts = d.fragmentedParts[:0]
		d.fragmentedMode = false
		return nil, 0, ErrShortPayload
	}

	// AU-headers-length (16 bits)
	headersLen := int(binary.BigEndian.Uint16(pkt.Payload))
	if headersLen == 0 {
		return nil, 0, ErrAUinvalidLength
	}
	payload := pkt.Payload[2:]

	// AU-headers
	dataLens, err := d.readAUHeaders(payload, headersLen)
	if err != nil {
		return nil, 0, err
	}
	pos := (headersLen / 8)
	if (headersLen % 8) != 0 {
		pos++
	}
	payload = payload[pos:]

	if d.adtsMode {
		if len(dataLens) != 1 {
			return nil, 0, ErrADTSmultipleAU
		}

		if len(payload) < int(dataLens[0]) {
			return nil, 0, ErrShortPayload
		}

		au := payload[:dataLens[0]]

		var pkts aac.ADTSPackets
		err := pkts.Unmarshal(au)
		if err != nil {
			return nil, 0, fmt.Errorf("unable to decode ADTS: %w", err)
		}

		if len(pkts) != 1 {
			return nil, 0, ErrMultipleADTS
		}

		return [][]byte{pkts[0].AU}, d.timeDecoder.Decode(pkt.Timestamp), nil
	}

	if d.fragmentedMode {
		return d.decodeFragmented(dataLens, payload, pkt)
	}
	return d.decodeUnfragmented(dataLens, payload, pkt)
}

func (d *Decoder) decodeFragmented(
	dataLens []uint64,
	payload []byte,
	pkt *rtp.Packet,
) ([][]byte, time.Duration, error) {
	if len(dataLens) != 1 {
		d.fragmentedParts = d.fragmentedParts[:0]
		d.fragmentedMode = false
		return nil, 0, ErrFragMultipleAU
	}

	if len(payload) < int(dataLens[0]) {
		return nil, 0, ErrShortPayload
	}

	d.fragmentedSize += int(dataLens[0])
	if d.fragmentedSize > aac.MaxAccessUnitSize {
		d.fragmentedParts = d.fragmentedParts[:0]
		d.fragmentedMode = false
		return nil, 0, AUsizeToBigError{AUsize: d.fragmentedSize}
	}

	d.fragmentedParts = append(d.fragmentedParts, payload[:dataLens[0]])

	if !pkt.Header.Marker {
		return nil, 0, ErrMorePacketsNeeded
	}

	ret := make([]byte, d.fragmentedSize)
	n := 0
	for _, p := range d.fragmentedParts {
		n += copy(ret[n:], p)
	}

	d.firstPacketParsed = true
	d.fragmentedParts = d.fragmentedParts[:0]
	d.fragmentedMode = false
	return [][]byte{ret}, d.timeDecoder.Decode(pkt.Timestamp), nil
}

func (d *Decoder) decodeUnfragmented( //nolint:gocognit
	dataLens []uint64,
	payload []byte,
	pkt *rtp.Packet,
) ([][]byte, time.Duration, error) {
	if pkt.Header.Marker { //nolint:nestif
		// AUs
		aus := make([][]byte, len(dataLens))
		for i, dataLen := range dataLens {
			if len(payload) < int(dataLen) {
				return nil, 0, ErrShortPayload
			}

			aus[i] = payload[:dataLen]
			payload = payload[dataLen:]
		}

		// some cameras wrap AUs with ADTS.
		if !d.firstPacketParsed {
			if len(aus) == 1 && len(aus[0]) >= 2 {
				if aus[0][0] == 0xFF && (aus[0][1]&0xF0) == 0xF0 {
					var pkts aac.ADTSPackets
					err := pkts.Unmarshal(aus[0])
					if err == nil && len(pkts) == 1 {
						d.adtsMode = true
						aus[0] = pkts[0].AU
					}
				}
			}
		}

		d.firstPacketParsed = true
		return aus, d.timeDecoder.Decode(pkt.Timestamp), nil
	}

	if len(dataLens) != 1 {
		return nil, 0, ErrFragMultipleAU
	}

	if len(payload) < int(dataLens[0]) {
		return nil, 0, ErrShortPayload
	}

	d.firstPacketParsed = true
	d.fragmentedSize = int(dataLens[0])
	d.fragmentedParts = append(d.fragmentedParts, payload[:dataLens[0]])
	d.fragmentedMode = true
	return nil, 0, ErrMorePacketsNeeded
}

func (d *Decoder) readAUHeaders(buf []byte, headersLen int) ([]uint64, error) {
	firstRead := false

	count := 0
	for i := 0; i < headersLen; {
		if i == 0 {
			i += d.SizeLength
			i += d.IndexLength
		} else {
			i += d.SizeLength
			i += d.IndexDeltaLength
		}
		count++
	}

	dataLens := make([]uint64, count)

	pos := 0
	i := 0

	for headersLen > 0 {
		dataLen, err := bits.ReadBits(buf, &pos, d.SizeLength)
		if err != nil {
			return nil, err
		}
		headersLen -= d.SizeLength

		if !firstRead { //nolint:nestif
			firstRead = true
			if d.IndexLength > 0 {
				auIndex, err := bits.ReadBits(buf, &pos, d.IndexLength)
				if err != nil {
					return nil, err
				}
				headersLen -= d.IndexLength

				if auIndex != 0 {
					return nil, ErrAUindexNotZero
				}
			}
		} else if d.IndexDeltaLength > 0 {
			auIndexDelta, err := bits.ReadBits(buf, &pos, d.IndexDeltaLength)
			if err != nil {
				return nil, err
			}
			headersLen -= d.IndexDeltaLength

			if auIndexDelta != 0 {
				return nil, ErrAUindexDeltaNotZero
			}
		}

		dataLens[i] = dataLen
		i++
	}

	return dataLens, nil
}
