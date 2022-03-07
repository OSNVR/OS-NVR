package rtpaac

import (
	"encoding/binary"
	"errors"
	"fmt"
	"nvr/pkg/video/gortsplib/pkg/rtptimedec"
	"time"

	"github.com/pion/rtp"
)

// ErrMorePacketsNeeded is returned when more packets are needed.
var ErrMorePacketsNeeded = errors.New("need more packets")

// Decoder is a RTP/AAC decoder.
type Decoder struct {
	timeDecoder          *rtptimedec.Decoder
	isDecodingFragmented bool
	fragmentedBuf        []byte
}

// NewDecoder allocates a Decoder.
func NewDecoder(clockRate int) *Decoder {
	return &Decoder{
		timeDecoder: rtptimedec.New(clockRate),
	}
}

// Errors.
var (
	ErrShortPayload    = errors.New("payload is too short")
	ErrAUinvalidLength = errors.New("invalid AU-headers-length")
	ErrAUindexNotZero  = errors.New("AU-index field is not zero")
	ErrFragMultipleAU  = errors.New("a fragmented packet can only contain one AU")
)

// Decode decodes AUs from a RTP/AAC packet.
// It returns the AUs and the PTS of the first AU.
// The PTS of subsequent AUs can be calculated by adding time.Second*1000/clockRate.
func (d *Decoder) Decode(pkt *rtp.Packet) ([][]byte, time.Duration, error) {
	if len(pkt.Payload) < 2 {
		d.isDecodingFragmented = false
		return nil, 0, ErrShortPayload
	}

	auHeadersLen := binary.BigEndian.Uint16(pkt.Payload)
	if (auHeadersLen % 16) != 0 {
		d.isDecodingFragmented = false
		return nil, 0, fmt.Errorf("%w (%d)", ErrAUinvalidLength, auHeadersLen)
	}
	pkt.Payload = pkt.Payload[2:]

	if d.isDecodingFragmented {
		return d.decodeFragmented(pkt, auHeadersLen)
	}
	return d.decodeUnfragmented(pkt, auHeadersLen)
}

func (d *Decoder) decodeFragmented(pkt *rtp.Packet, auHeadersLen uint16) ([][]byte, time.Duration, error) {
	// we are decoding a fragmented AU

	if auHeadersLen != 16 {
		return nil, 0, ErrFragMultipleAU
	}

	// AU-header
	header := binary.BigEndian.Uint16(pkt.Payload)
	dataLen := header >> 3
	auIndex := header & 0x03
	if auIndex != 0 {
		return nil, 0, ErrAUindexNotZero
	}
	pkt.Payload = pkt.Payload[2:]

	if len(pkt.Payload) < int(dataLen) {
		return nil, 0, ErrShortPayload
	}

	d.fragmentedBuf = append(d.fragmentedBuf, pkt.Payload...)

	if !pkt.Header.Marker {
		return nil, 0, ErrMorePacketsNeeded
	}

	d.isDecodingFragmented = false
	return [][]byte{d.fragmentedBuf}, d.timeDecoder.Decode(pkt.Timestamp), nil
}

func (d *Decoder) decodeUnfragmented(pkt *rtp.Packet, auHeadersLen uint16) ([][]byte, time.Duration, error) {
	if pkt.Header.Marker {
		// AU-headers
		// AAC headers are 16 bits, where
		// * 13 bits are data size
		// * 3 bits are AU index
		headerCount := auHeadersLen / 16
		var dataLens []uint16
		for i := 0; i < int(headerCount); i++ {
			if len(pkt.Payload[i*2:]) < 2 {
				return nil, 0, ErrShortPayload
			}

			header := binary.BigEndian.Uint16(pkt.Payload[i*2:])
			dataLen := header >> 3
			auIndex := header & 0x03
			if auIndex != 0 {
				return nil, 0, ErrAUindexNotZero
			}

			dataLens = append(dataLens, dataLen)
		}
		pkt.Payload = pkt.Payload[headerCount*2:]

		// AUs
		aus := make([][]byte, len(dataLens))
		for i, dataLen := range dataLens {
			if len(pkt.Payload) < int(dataLen) {
				return nil, 0, ErrShortPayload
			}

			aus[i] = pkt.Payload[:dataLen]
			pkt.Payload = pkt.Payload[dataLen:]
		}

		return aus, d.timeDecoder.Decode(pkt.Timestamp), nil
	}

	if auHeadersLen != 16 {
		return nil, 0, ErrFragMultipleAU
	}

	// AU-header
	header := binary.BigEndian.Uint16(pkt.Payload)
	dataLen := header >> 3
	auIndex := header & 0x03
	if auIndex != 0 {
		return nil, 0, ErrAUindexNotZero
	}
	pkt.Payload = pkt.Payload[2:]

	if len(pkt.Payload) < int(dataLen) {
		return nil, 0, ErrShortPayload
	}

	d.fragmentedBuf = append(d.fragmentedBuf, pkt.Payload...)

	d.isDecodingFragmented = true
	return nil, 0, ErrMorePacketsNeeded
}