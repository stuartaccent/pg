package web

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for psql, so the tests need no shell.
func TestMain(m *testing.M) {
	if os.Getenv("WEB_HELPER") == "psql" {
		// Reads a line, then says what it got and the terminal's size.
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		rows, cols := termSize()
		fmt.Printf("got:%s size:%d %d\n", line, rows, cols)
		if line == "wait" {
			time.Sleep(30 * time.Second)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
