package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// LogLine defines the relevant fields we want to parse from the json output.
type LogLine struct {
	Action, Output string
}

func main() {
	logFile := os.Getenv("TEST_JSON_REPORT")
	fmt.Printf("# Checking for panics in %s\n", logFile)

	if os.Getenv("CI_JOB_STATUS") != "failed" {
		return
	}

	f, err := os.Open(logFile)
	if err != nil {
		return
	}
	defer f.Close()

	decoder := json.NewDecoder(f)

	var printIt bool
	var inBacktrace bool
	for decoder.More() {
		var line LogLine
		if err := decoder.Decode(&line); err != nil {
			return
		}
		if line.Action != "output" {
			if inBacktrace {
				inBacktrace = false
				fmt.Println("") // Print a trailing new line to separate panics.
			}
			continue
		}
		if strings.HasPrefix(line.Output, "panic:") {
			inBacktrace = true
			if !printIt {
				fmt.Printf("\x1b[0Ksection_start:%v:panic_stack_traces[collapsed=true]\r\x1b[0K\x1b[0;31mPanic stack traces\x1b[0m\n", time.Now().Unix())
			}
			printIt = true
		}
		if inBacktrace {
			fmt.Print(line.Output)
		}
	}
	if printIt {
		fmt.Printf("\x1b[0Ksection_end:%v:panic_stack_traces\r\x1b[0K\n", time.Now().Unix())
	}
}
