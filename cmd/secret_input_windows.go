package cmd

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

var readConsoleInput = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

type secretInput struct {
	ctx       context.Context
	handle    windows.Handle
	pending   []byte
	surrogate uint16
}

func newSecretInput(ctx context.Context, input *os.File) io.Reader {
	return &secretInput{ctx: ctx, handle: windows.Handle(input.Fd())}
}

func (input *secretInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := input.ctx.Err(); err != nil {
			return 0, err
		}
		if len(input.pending) != 0 {
			n := copy(p, input.pending)
			input.pending = input.pending[n:]
			return n, nil
		}
		event, err := windows.WaitForSingleObject(input.handle, 100)
		if err != nil {
			return 0, err
		}
		if event == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		if event != windows.WAIT_OBJECT_0 {
			return 0, fmt.Errorf("unexpected console input wait result: %d", event)
		}
		// INPUT_RECORD consists of a WORD type, 2 bytes of padding and a
		// 16-byte union. Consume records, including key-up/resize events,
		// rather than calling ReadConsole which could block again on them.
		var record [20]byte
		var count uint32
		ok, _, callErr := readConsoleInput.Call(uintptr(input.handle), uintptr(unsafe.Pointer(&record[0])), 1, uintptr(unsafe.Pointer(&count)))
		if ok == 0 {
			return 0, callErr
		}
		if count == 0 || binary.LittleEndian.Uint16(record[:2]) != 1 { // KEY_EVENT
			continue
		}
		keyDown := binary.LittleEndian.Uint32(record[4:8]) != 0
		virtualKey := binary.LittleEndian.Uint16(record[10:12])
		if !keyDown && virtualKey != 0x12 { // Alt-numpad input arrives on Alt release.
			continue
		}
		repeat := int(binary.LittleEndian.Uint16(record[8:10]))
		if repeat == 0 {
			repeat = 1
		}
		character := binary.LittleEndian.Uint16(record[14:16])
		if character == 0 {
			sequences := map[uint16]string{0x23: "\x1b[F", 0x24: "\x1b[H", 0x25: "\x1b[D", 0x27: "\x1b[C", 0x2e: "\x1b[3~"}
			input.pending = bytes.Repeat([]byte(sequences[virtualKey]), repeat)
			continue
		}
		if character >= 0xd800 && character <= 0xdbff {
			input.surrogate = character
			continue
		}
		value := rune(character)
		if input.surrogate != 0 {
			value = utf16.DecodeRune(rune(input.surrogate), value)
			input.surrogate = 0
		}
		input.pending = bytes.Repeat(utf8.AppendRune(nil, value), repeat)
	}
}
