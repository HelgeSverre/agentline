package cli

import (
	"io"
	"os"
)

type style struct {
	color bool
}

func newStyle(w io.Writer) style {
	file, ok := w.(*os.File)
	if !ok {
		return style{}
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return style{}
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return style{}
	}
	return style{color: true}
}

const (
	ansiBold  = "\x1b[1m"
	ansiReset = "\x1b[0m"
)

func (s style) bold(value string) string {
	if !s.color {
		return value
	}
	return ansiBold + value + ansiReset
}