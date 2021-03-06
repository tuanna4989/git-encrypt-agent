package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

var parseContinue = errors.New("CONTINUE")

type parseProcessingState int

const (
	UNINITIALIZED parseProcessingState = iota
	ELEM_COUNT_INITIALIZING
	ELEM_COUNT_INITIALIZED
	ELEM_INITIALIZING
	ELEM_STRING_SIZE_INITIALIZING
	ELEM_STRING_SIZE_INITIALIZED
	ELEM_STRING_INITIALIZING
	ELEM_STRING_INITIALIZED
	INITIALIZED
)

type Element struct {
	kind resultTypeCode
	size int
	data []byte
}

func NewElement(size int, data []byte) *Element {
	e := new(Element)
	e.size = size
	e.data = data
	return e
}

type Parser struct {
	// 全要素数
	count int

	//要素の配列
	elements []*Element

	// 処理状態
	state parseProcessingState
	// 改行コード処理後のステータス指定
	nextState parseProcessingState

	// 初期化中の要素のインデックス番号
	tmpIndex int
	// 初期化中の数値データ
	tmpNumber []byte
	// 初期化中の文字列データ
	tmpString []byte
	// 初期化中の改行コード処理状態
	tmpCrlfState int
}

func NewParser() *Parser {
	r := new(Parser)
	return r
}

func (r *Parser) Parse(conn net.Conn) error {
	for {
		buf := make([]byte, 512)
		nr, err := conn.Read(buf)
		if err != nil {
			if err == io.EOF {
				r.eof()
				break
			} else {
				return err
			}
		}
		err = r.write(buf[0:nr])
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Parser) eof() error {
	if r.state != ELEM_INITIALIZING {
		return r.protocolError("EOF Error")
	}
	r.state = INITIALIZED
	return nil
}

func (r *Parser) write(buf []byte) error {
	length := len(buf)
	if length == 0 {
		return nil
	}
	switch r.state {
	case UNINITIALIZED:
		// 42 == *
		if buf[0:1][0] == 42 {
			r.state = ELEM_COUNT_INITIALIZING
			return r.write(buf[1:])
		}
		r.count = 1
		r.state = ELEM_INITIALIZING
		r.elements = make([]*Element, 1, 1)
		return r.write(buf)
	case ELEM_COUNT_INITIALIZING:
		return r.writeElementsCount(buf)
	case ELEM_INITIALIZING:
		if r.count <= r.tmpIndex {
			break
		}
		// 36 == $, 43 == +, 45 == -,
		switch buf[0] {
		case 36:
			r.state = ELEM_STRING_SIZE_INITIALIZING
			return r.write(buf[1:])
		case 43:
			return r.writeSimpleStringSize(buf[1:])
		case 45:
			return r.writeErrorStringSize(buf[1:])
		default:
			break
		}
	case ELEM_STRING_SIZE_INITIALIZING:
		return r.writeBinaryStringSize(buf)
	case ELEM_STRING_INITIALIZING:
		return r.writeBinaryString(buf)
	case ELEM_COUNT_INITIALIZED, ELEM_STRING_SIZE_INITIALIZED, ELEM_STRING_INITIALIZED:
		return r.writeCRLF(buf)
	}
	return r.protocolError("Unknown write error")
}

// private
func (r *Parser) writeElementsCount(buf []byte) error {
	count, newBuf, err := r.readInteger(buf)
	if err != nil {
		if err == parseContinue {
			return nil
		}
		return err
	}
	r.count = count
	r.state = ELEM_COUNT_INITIALIZED
	r.nextState = ELEM_INITIALIZING
	r.elements = make([]*Element, count, count)
	return r.write(newBuf)
}

func (r *Parser) readInteger(buf []byte) (int, []byte, error) {
	length := len(buf)
	for i := 0; i < length; i++ {
		b := buf[i : i+1][0]
		if r.tmpNumber == nil {
			if b == 45 {
				r.tmpNumber = append(r.tmpNumber, b)
				continue
			}
		}
		if b >= 48 && b <= 57 {
			r.tmpNumber = append(r.tmpNumber, b)
			continue
		}
		// \r => 13
		if b == 13 {
			number, err := strconv.Atoi(string(r.tmpNumber))
			if err == nil {
				r.tmpNumber = nil
				newBuf := buf[i:]
				return number, newBuf, nil
			}
		}
		return 0, nil, r.protocolError("Invalid charactor in number")
	}
	return 0, nil, parseContinue
}

func (r *Parser) writeSimpleStringSize(buf []byte) error {
	length := len(buf)
	if r.tmpString == nil {
		r.tmpString = make([]byte, 0)
	}
	for i := 0; i < length; i++ {
		b := buf[i : i+1][0]
		// \r => 13
		if b == 13 {
			r.tmpString = append(r.tmpString, buf[:i]...)
			r.elements[r.tmpIndex] = NewElement(len(r.tmpString), r.tmpString)
			r.elements[r.tmpIndex].kind = SIMPLE_STRING
			r.tmpString = nil
			r.tmpIndex++
			return nil
		}
	}
	r.tmpString = append(r.tmpString, buf...)
	return parseContinue
}

func (r *Parser) writeErrorStringSize(buf []byte) error {
	length := len(buf)
	if r.tmpString == nil {
		r.tmpString = make([]byte, 0)
	}
	for i := 0; i < length; i++ {
		b := buf[i : i+1][0]
		// \r => 13
		if b == 13 {
			r.tmpString = append(r.tmpString, buf[:i]...)
			r.elements[r.tmpIndex] = NewElement(len(r.tmpString), r.tmpString)
			r.elements[r.tmpIndex].kind = ERROR_STRING
			r.tmpString = nil
			r.tmpIndex++
			return nil
		}
	}
	r.tmpString = append(r.tmpString, buf...)
	return parseContinue
}

func (r *Parser) writeBinaryStringSize(buf []byte) error {
	size, newBuf, err := r.readInteger(buf)
	if err != nil {
		if err == parseContinue {
			return nil
		}
		return err
	}
	r.elements[r.tmpIndex] = &Element{
		kind: BINARY_STRING,
		size: size,
	}
	r.state = ELEM_STRING_SIZE_INITIALIZED
	if size == -1 {
		r.nextState = ELEM_INITIALIZING
	} else {
		r.nextState = ELEM_STRING_INITIALIZING
	}
	return r.write(newBuf)
}

func (r *Parser) writeBinaryString(buf []byte) error {
	length := len(buf)
	element := r.elements[r.tmpIndex]
	remaining := element.size - len(element.data)
	if length >= remaining {
		element.data = append(element.data, buf[0:remaining]...)
		r.state = ELEM_STRING_INITIALIZED
		r.nextState = ELEM_INITIALIZING
		r.tmpIndex++
		return r.write(buf[remaining:])
	}
	element.data = append(element.data, buf...)
	return nil
}

func (r *Parser) writeCRLF(buf []byte) error {
	length := len(buf)
	for i := 0; i < length; i++ {
		b := buf[i : i+1][0]
		if b == 13 && r.tmpCrlfState == 0 {
			r.tmpCrlfState = 1
			if length == i+1 {
				return nil
			}
			continue
		} else if b == 10 && r.tmpCrlfState == 1 {
			r.tmpCrlfState = 0
			r.state = r.nextState
			r.nextState = 0
			return r.write(buf[i+1:])
		}
		break
	}
	return r.protocolError("Invalid charactor for new line")
}

func (r *Parser) protocolError(message string) error {
	return fmt.Errorf("Protocol Error: %s", message)
}
