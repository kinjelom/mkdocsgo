package web

import (
	"io"
	"strconv"
)

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func copyTo(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }
