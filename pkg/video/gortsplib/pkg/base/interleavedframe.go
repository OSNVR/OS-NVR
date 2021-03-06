package base

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	interleavedFrameMagicByte = 0x24
)

// ReadInterleavedFrameOrRequest reads an InterleavedFrame or a Response.
func ReadInterleavedFrameOrRequest(
	frame *InterleavedFrame,
	maxPayloadSize int,
	req *Request,
	br *bufio.Reader,
) (interface{}, error) {
	b, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	if err := br.UnreadByte(); err != nil {
		return nil, err
	}

	if b == interleavedFrameMagicByte {
		err := frame.Read(maxPayloadSize, br)
		if err != nil {
			return nil, err
		}
		return frame, err
	}

	err = req.Read(br)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// ReadInterleavedFrameOrResponse reads an InterleavedFrame or a Response.
func ReadInterleavedFrameOrResponse(
	frame *InterleavedFrame,
	maxPayloadSize int,
	res *Response,
	br *bufio.Reader,
) (interface{}, error) {
	b, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	if err := br.UnreadByte(); err != nil {
		return nil, err
	}

	if b == interleavedFrameMagicByte {
		err := frame.Read(maxPayloadSize, br)
		if err != nil {
			return nil, err
		}
		return frame, err
	}

	err = res.Read(br)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// InterleavedFrame is an interleaved frame, and allows to transfer binary data
// within RTSP/TCP connections. It is used to send and receive RTP packets with TCP.
type InterleavedFrame struct {
	// Channel ID.
	Channel int
	Payload []byte
}

// ErrInvalidMagicByte invalid magic byte.
var ErrInvalidMagicByte = errors.New("invalid magic byte")

// PayloadToBigError .
type PayloadToBigError struct {
	PayloadLen     int
	MaxPayloadSize int
}

func (e PayloadToBigError) Error() string {
	return fmt.Sprintf("payload size (%d) greater than maximum allowed (%d)",
		e.PayloadLen, e.MaxPayloadSize)
}

// Read decodes an interleaved frame.
func (f *InterleavedFrame) Read(maxPayloadSize int, br *bufio.Reader) error {
	var header [4]byte
	_, err := io.ReadFull(br, header[:])
	if err != nil {
		return err
	}

	if header[0] != interleavedFrameMagicByte {
		return fmt.Errorf("%w (0x%.2x)", ErrInvalidMagicByte, header[0])
	}

	payloadLen := int(binary.BigEndian.Uint16(header[2:]))
	if payloadLen > maxPayloadSize {
		return PayloadToBigError{PayloadLen: payloadLen, MaxPayloadSize: maxPayloadSize}
	}

	f.Channel = int(header[1])
	f.Payload = make([]byte, payloadLen)

	_, err = io.ReadFull(br, f.Payload)
	if err != nil {
		return err
	}
	return nil
}

// MarshalSize returns the size of an InterleavedFrame.
func (f InterleavedFrame) MarshalSize() int {
	return 4 + len(f.Payload)
}

// MarshalTo writes an InterleavedFrame.
func (f InterleavedFrame) MarshalTo(buf []byte) (int, error) {
	pos := 0

	pos += copy(buf[pos:], []byte{0x24, byte(f.Channel)})

	binary.BigEndian.PutUint16(buf[pos:], uint16(len(f.Payload)))
	pos += 2

	pos += copy(buf[pos:], f.Payload)

	return pos, nil
}

// Marshal writes an InterleavedFrame.
func (f InterleavedFrame) Marshal() ([]byte, error) {
	buf := make([]byte, f.MarshalSize())
	_, err := f.MarshalTo(buf)
	return buf, err
}
