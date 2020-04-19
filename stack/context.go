// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

//go:generate stringer -type state

package stack

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Context is a parsing context.
//
// It contains the deduced GOROOT and GOPATH, if guesspaths is true.
type Context struct {
	// Goroutines is the Goroutines found.
	//
	// They are in the order that they were printed.
	Goroutines []*Goroutine

	// GOROOT is the GOROOT as detected in the traceback, not the on the host.
	//
	// It can be empty if no root was determined, for example the traceback
	// contains only non-stdlib source references.
	//
	// Empty is guesspaths was false.
	GOROOT string
	// GOPATHs is the GOPATH as detected in the traceback, with the value being
	// the corresponding path mapped to the host.
	//
	// It can be empty if only stdlib code is in the traceback or if no local
	// sources were matched up. In the general case there is only one entry in
	// the map.
	//
	// Nil is guesspaths was false.
	GOPATHs map[string]string

	// localgoroot is GOROOT with "/" as path separator. No trailing "/".
	localgoroot string
	// localgopaths is GOPATH with "/" as path separator. No trailing "/".
	localgopaths []string
}

// ParseDump processes the output from runtime.Stack().
//
// Returns nil *Context if no stack trace was detected.
//
// It pipes anything not detected as a panic stack trace from r into out. It
// assumes there is junk before the actual stack trace. The junk is streamed to
// out.
//
// If guesspaths is false, no guessing of GOROOT and GOPATH is done, and Call
// entites do not have LocalSrcPath and IsStdlib filled in. If true, be warned
// that file presence is done, which means some level of disk I/O.
func ParseDump(r io.Reader, out io.Writer, guesspaths bool) (*Context, error) {
	goroutines, err := parseDump(r, out)
	if len(goroutines) == 0 {
		return nil, err
	}
	c := &Context{
		Goroutines:   goroutines,
		localgoroot:  strings.Replace(runtime.GOROOT(), "\\", "/", -1),
		localgopaths: getGOPATHs(),
	}
	nameArguments(goroutines)
	// Corresponding local values on the host for Context.
	if guesspaths {
		c.findRoots()
		for _, r := range c.Goroutines {
			// Note that this is important to call it even if
			// c.GOROOT == c.localgoroot.
			r.updateLocations(c.GOROOT, c.localgoroot, c.GOPATHs)
		}
	}
	return c, err
}

// Private stuff.

const (
	lockedToThread   = "locked to thread"
	elided           = "...additional frames elided..."
	raceHeaderFooter = "=================="
	raceHeader       = "WARNING: DATA RACE"
)

// These are effectively constants.
var (
	// TODO(maruel): Handle corrupted stack cases:
	// - missed stack barrier
	// - found next stack barrier at 0x123; expected
	// - runtime: unexpected return pc for FUNC_NAME called from 0x123

	reRoutineHeader = regexp.MustCompile("^([ \t]*)goroutine (\\d+) \\[([^\\]]+)\\]\\:$")
	reMinutes       = regexp.MustCompile("^(\\d+) minutes$")
	reUnavail       = regexp.MustCompile("^(?:\t| +)goroutine running on other thread; stack unavailable")
	// See gentraceback() in src/runtime/traceback.go for more information.
	// - Sometimes the source file comes up as "<autogenerated>". It is the
	//   compiler than generated these, not the runtime.
	// - The tab may be replaced with spaces when a user copy-paste it, handle
	//   this transparently.
	// - "runtime.gopanic" is explicitly replaced with "panic" by gentraceback().
	// - The +0x123 byte offset is printed when frame.pc > _func.entry. _func is
	//   generated by the linker.
	// - The +0x123 byte offset is not included with generated code, e.g. unnamed
	//   functions "func·006()" which is generally go func() { ... }()
	//   statements. Since the _func is generated at runtime, it's probably why
	//   _func.entry is not set.
	// - C calls may have fp=0x123 sp=0x123 appended. I think it normally happens
	//   when a signal is not correctly handled. It is printed with m.throwing>0.
	//   These are discarded.
	// - For cgo, the source file may be "??".
	reFile = regexp.MustCompile("^(?:\t| +)(\\?\\?|\\<autogenerated\\>|.+\\.(?:c|go|s))\\:(\\d+)(?:| \\+0x[0-9a-f]+)(?:| fp=0x[0-9a-f]+ sp=0x[0-9a-f]+(?:| pc=0x[0-9a-f]+))$")
	// Sadly, it doesn't note the goroutine number so we could cascade them per
	// parenthood.
	reCreated = regexp.MustCompile("^created by (.+)$")
	reFunc    = regexp.MustCompile("^(.+)\\((.*)\\)$")

	// See https://github.com/llvm/llvm-project/blob/master/compiler-rt/lib/tsan/rtl/tsan_report.cc
	// for the code generating these messages. Please note only the block in
	//   #else  // #if !SANITIZER_GO
	// is used.
	// TODO(maruel): "    [failed to restore the stack]\n\n"
	// TODO(maruel): "Global var %s of size %zu at %p declared at %s:%zu\n"
	reRaceOperationHeader             = regexp.MustCompile("^(Read|Write) at (0x[0-9a-f]+) by goroutine (\\d+):$")
	reRacePreviousOperationHeader     = regexp.MustCompile("^Previous (read|write) at (0x[0-9a-f]+) by goroutine (\\d+):$")
	reRacePreviousOperationMainHeader = regexp.MustCompile("^Previous (read|write) at (0x[0-9a-f]+) by main goroutine:$")
	reRaceGoroutine                   = regexp.MustCompile("^Goroutine (\\d+) \\((running|finished)\\) created at:$")
)

func parseDump(r io.Reader, out io.Writer) ([]*Goroutine, error) {
	scanner := bufio.NewScanner(r)
	scanner.Split(scanLines)
	// Do not enable race detection parsing yet, since it cannot be returned in
	// Context at the moment.
	s := scanningState{}
	for scanner.Scan() {
		line, err := s.scan(scanner.Text())
		if line != "" {
			_, _ = io.WriteString(out, line)
		}
		if err != nil {
			return s.goroutines, err
		}
	}
	return s.goroutines, scanner.Err()
}

// scanLines is similar to bufio.ScanLines except that it:
//     - doesn't drop '\n'
//     - doesn't strip '\r'
//     - returns when the data is bufio.MaxScanTokenSize bytes
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[0 : i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	if len(data) >= bufio.MaxScanTokenSize {
		// Returns the line even if it is not at EOF nor has a '\n', otherwise the
		// scanner will return bufio.ErrTooLong which is definitely not what we
		// want.
		return len(data), data, nil
	}
	return 0, nil, nil
}

// state is the state of the scan to detect and process a stack trace.
type state int

// Initial state is normal. Other states are when a stack trace is detected.
const (
	// Outside a stack trace.
	// to: gotRoutineHeader, raceHeader1
	normal state = iota

	// Panic stack trace:

	// Empty line between goroutines.
	// from: gotFileCreated, gotFileFunc
	// to: gotRoutineHeader, normal
	betweenRoutine
	// Goroutine header was found, e.g. "goroutine 1 [running]:"
	// from: normal
	// to: gotUnavail, gotFunc
	gotRoutineHeader
	// Function call line was found, e.g. "main.main()"
	// from: gotRoutineHeader
	// to: gotFile
	gotFunc
	// Goroutine creation line was found, e.g. "created by main.glob..func4"
	// from: gotFileFunc
	// to: gotFileCreated
	gotCreated
	// File header was found, e.g. "\t/foo/bar/baz.go:116 +0x35"
	// from: gotFunc
	// to: gotFunc, gotCreated, betweenRoutine, normal
	gotFileFunc
	// File header was found, e.g. "\t/foo/bar/baz.go:116 +0x35"
	// from: gotCreated
	// to: betweenRoutine, normal
	gotFileCreated
	// State when the goroutine stack is instead is reUnavail.
	// from: gotRoutineHeader
	// to: betweenRoutine, gotCreated
	gotUnavail

	// Race detector:

	// Got "=================="
	// from: normal
	// to: normal, gotRaceHeader
	gotRaceHeader1
	// Got "WARNING: DATA RACE"
	// from: gotRaceHeader1
	// to: normal, gotRaceOperationHeader
	gotRaceHeader
	// A race operation was found, e.g. "Read at 0x00c0000e4030 by goroutine 7:"
	// from: gotRaceHeader
	// to: normal, gotRaceOperationFunc
	gotRaceOperationHeader
	// Function that caused the race, e.g. "  main.panicRace.func1()"
	// from: gotRaceOperationHeader
	// to: normal, gotRaceOperationFile
	gotRaceOperationFunc
	// Function that caused the race, e.g. "  main.panicRace.func1()"
	// from: gotRaceOperationFunc
	// to: normal, betweenRaces
	gotRaceOperationFile
	// Goroutine header, e.g. "Goroutine 7 (running) created at:"
	// from: betweenRaces
	// to: normal, gotRaceOperationHeader
	gotRaceGoroutineHeader
	// Function that caused the race, e.g. "  main.panicRace.func1()"
	// from: gotRaceGoroutineHeader
	// to: normal, gotRaceGoroutineFile
	gotRaceGoroutineFunc
	// Function that caused the race, e.g. "  main.panicRace.func1()"
	// from: gotRaceGoroutineFunc
	// to: normal, betweenRaces
	gotRaceGoroutineFile
	// Empty line between goroutines.
	// from: gotRaceOperationFile
	// to: normal, gotRaceOperationHeader
	betweenRaces
)

type raceOp struct {
	write bool
	addr  uint64
	id    int
}

// scanningState is the state of the scan to detect and process a stack trace
// and stores the traces found.
type scanningState struct {
	// Determines if race detection is enabled. Currently false since scan()
	// would swallow the race detector output, but the data is not part of
	// Context yet.
	raceDetectionEnabled bool

	// goroutines contains all the goroutines found.
	goroutines []*Goroutine

	state  state
	prefix string
	races  []raceOp
}

// scan scans one line, updates goroutines and move to the next state.
func (s *scanningState) scan(line string) (string, error) {
	/* This is very useful to debug issues in the state machine.
	defer func() {
		log.Printf("scan(%q) -> %s", line, s.state)
	}()
	//*/
	var cur *Goroutine
	if len(s.goroutines) != 0 {
		cur = s.goroutines[len(s.goroutines)-1]
	}
	trimmed := line
	if strings.HasSuffix(line, "\r\n") {
		trimmed = line[:len(line)-2]
	} else if strings.HasSuffix(line, "\n") {
		trimmed = line[:len(line)-1]
	} else {
		// There's two cases:
		// - It's the end of the stream and it's not terminating with EOL character.
		// - The line is longer than bufio.MaxScanTokenSize
		if s.state == normal {
			return line, nil
		}
		// Let it flow. It's possible the last line was trimmed and we still want to parse it.
	}

	if trimmed != "" && s.prefix != "" {
		// This can only be the case if s.state != normal or the line is empty.
		if !strings.HasPrefix(trimmed, s.prefix) {
			prefix := s.prefix
			s.state = normal
			s.prefix = ""
			return "", fmt.Errorf("inconsistent indentation: %q, expected %q", trimmed, prefix)
		}
		trimmed = trimmed[len(s.prefix):]
	}

	switch s.state {
	case normal:
		// We could look for '^panic:' but this is more risky, there can be a lot
		// of junk between this and the stack dump.
		fallthrough
	case betweenRoutine:
		// Look for a goroutine header.
		if match := reRoutineHeader.FindStringSubmatch(trimmed); match != nil {
			if id, err := strconv.Atoi(match[2]); err == nil {
				// See runtime/traceback.go.
				// "<state>, \d+ minutes, locked to thread"
				items := strings.Split(match[3], ", ")
				sleep := 0
				locked := false
				for i := 1; i < len(items); i++ {
					if items[i] == lockedToThread {
						locked = true
						continue
					}
					// Look for duration, if any.
					if match2 := reMinutes.FindStringSubmatch(items[i]); match2 != nil {
						sleep, _ = strconv.Atoi(match2[1])
					}
				}
				g := &Goroutine{
					Signature: Signature{
						State:    items[0],
						SleepMin: sleep,
						SleepMax: sleep,
						Locked:   locked,
					},
					ID:    id,
					First: len(s.goroutines) == 0,
				}
				// Increase performance by always allocating 4 goroutines minimally.
				if s.goroutines == nil {
					s.goroutines = make([]*Goroutine, 0, 4)
				}
				s.goroutines = append(s.goroutines, g)
				s.state = gotRoutineHeader
				s.prefix = match[1]
				return "", nil
			}
		}
		// Switch to race detection mode.
		if s.raceDetectionEnabled && trimmed == raceHeaderFooter {
			// TODO(maruel): We should buffer it in case the next line is not a
			// WARNING so we can output it back.
			s.state = gotRaceHeader1
			return "", nil
		}
		// Fallthrough.
		s.state = normal
		s.prefix = ""
		return line, nil

	case gotRoutineHeader:
		if reUnavail.MatchString(trimmed) {
			// Generate a fake stack entry.
			cur.Stack.Calls = []Call{{SrcPath: "<unavailable>"}}
			// Next line is expected to be an empty line.
			s.state = gotUnavail
			return "", nil
		}
		c := Call{}
		if found, err := parseFunc(&c, trimmed); found {
			// Increase performance by always allocating 4 calls minimally.
			if cur.Stack.Calls == nil {
				cur.Stack.Calls = make([]Call, 0, 4)
			}
			cur.Stack.Calls = append(cur.Stack.Calls, c)
			s.state = gotFunc
			return "", err
		}
		return "", fmt.Errorf("expected a function after a goroutine header, got: %q", strings.TrimSpace(trimmed))

	case gotFunc:
		// cur.Stack.Calls is guaranteed to have at least one item.
		if found, err := parseFile(&cur.Stack.Calls[len(cur.Stack.Calls)-1], trimmed); err != nil {
			return "", err
		} else if !found {
			return "", fmt.Errorf("expected a file after a function, got: %q", strings.TrimSpace(trimmed))
		}
		s.state = gotFileFunc
		return "", nil

	case gotCreated:
		if found, err := parseFile(&cur.CreatedBy, trimmed); err != nil {
			return "", err
		} else if !found {
			return "", fmt.Errorf("expected a file after a created line, got: %q", trimmed)
		}
		s.state = gotFileCreated
		return "", nil

	case gotFileFunc:
		if match := reCreated.FindStringSubmatch(trimmed); match != nil {
			cur.CreatedBy.Func.Raw = match[1]
			s.state = gotCreated
			return "", nil
		}
		if elided == trimmed {
			cur.Stack.Elided = true
			// TODO(maruel): New state.
			return "", nil
		}
		c := Call{}
		if found, err := parseFunc(&c, trimmed); found {
			// Increase performance by always allocating 4 calls minimally.
			if cur.Stack.Calls == nil {
				cur.Stack.Calls = make([]Call, 0, 4)
			}
			cur.Stack.Calls = append(cur.Stack.Calls, c)
			s.state = gotFunc
			return "", err
		}
		if trimmed == "" {
			s.state = betweenRoutine
			return "", nil
		}
		// Back to normal state.
		s.state = normal
		s.prefix = ""
		return line, nil

	case gotFileCreated:
		if trimmed == "" {
			s.state = betweenRoutine
			return "", nil
		}
		s.state = normal
		s.prefix = ""
		return line, nil

	case gotUnavail:
		if trimmed == "" {
			s.state = betweenRoutine
			return "", nil
		}
		if match := reCreated.FindStringSubmatch(trimmed); match != nil {
			cur.CreatedBy.Func.Raw = match[1]
			s.state = gotCreated
			return "", nil
		}
		return "", fmt.Errorf("expected empty line after unavailable stack, got: %q", strings.TrimSpace(trimmed))

	case gotRaceHeader1:
		if raceHeader == trimmed {
			// TODO(maruel): We should buffer it in case the next line is not a
			// WARNING so we can output it back.
			s.state = gotRaceHeader
			return "", nil
		}
		s.state = normal
		return line, nil

	case gotRaceHeader:
		if match := reRaceOperationHeader.FindStringSubmatch(trimmed); match != nil {
			w := match[1] == "Write"
			addr, err := strconv.ParseUint(match[2], 0, 64)
			if err != nil {
				return "", fmt.Errorf("failed to parse address on line: %q", strings.TrimSpace(trimmed))
			}
			id, err := strconv.Atoi(match[3])
			if err != nil {
				return "", fmt.Errorf("failed to parse goroutine id on line: %q", strings.TrimSpace(trimmed))
			}
			// Increase performance by always allocating 4 race operations minimally.
			if s.races == nil {
				s.races = make([]raceOp, 0, 4)
			}
			s.races = append(s.races, raceOp{w, addr, id})
			s.state = gotRaceOperationHeader
			return "", nil
		}
		s.state = normal
		return line, nil

	case gotRaceOperationHeader:
		c := Call{}
		if found, err := parseFunc(&c, trimmed); found {
			// TODO(maruel): Figure out.
			//cur.Stack.Calls = append(cur.Stack.Calls, c)
			s.state = gotRaceOperationFunc
			return "", err
		}
		return "", fmt.Errorf("expected a function after a race operation, got: %q", trimmed)

	case gotRaceGoroutineHeader:
		c := Call{}
		if found, err := parseFunc(&c, strings.TrimLeft(trimmed, "\t ")); found {
			// Increase performance by always allocating 4 calls minimally.
			if cur.Stack.Calls == nil {
				cur.Stack.Calls = make([]Call, 0, 4)
			}
			cur.Stack.Calls = append(cur.Stack.Calls, c)
			s.state = gotRaceGoroutineFunc
			return "", err
		}
		return "", fmt.Errorf("expected a function after a race operation, got: %q", trimmed)

	case gotRaceOperationFunc:
		// cur.Stack.Calls is guaranteed to have at least one item.
		// TODO(maruel): Bug, should be cur.Stack.Calls[len(cur.Stack.Calls)-1] but
		// s.goroutine isn't initialized properly.
		c := Call{}
		if found, err := parseFile(&c, trimmed); err != nil {
			return "", err
		} else if !found {
			return "", fmt.Errorf("expected a file after a race function, got: %q", trimmed)
		}
		s.state = gotRaceOperationFile
		return "", nil

	case gotRaceGoroutineFunc:
		// cur.Stack.Calls is guaranteed to have at least one item.
		if found, err := parseFile(&cur.Stack.Calls[len(cur.Stack.Calls)-1], trimmed); err != nil {
			return "", err
		} else if !found {
			return "", fmt.Errorf("expected a file after a race function, got: %q", trimmed)
		}
		s.state = gotRaceGoroutineFile
		return "", nil

	case gotRaceOperationFile:
		if trimmed == "" {
			s.state = betweenRaces
			return "", nil
		}
		return "", fmt.Errorf("expected an empty line after a race file, got: %q", trimmed)

	case gotRaceGoroutineFile:
		if trimmed == "" {
			s.state = betweenRaces
			return "", nil
		}
		if trimmed == raceHeaderFooter {
			// Done.
			s.state = normal
			return "", nil
		}
		c := Call{}
		if found, err := parseFunc(&c, strings.TrimLeft(trimmed, "\t ")); found {
			// TODO(maruel): Process match.
			s.state = gotRaceGoroutineFunc
			return "", err
		}
		return "", fmt.Errorf("expected a function or the end after a race file, got: %q", trimmed)

	case betweenRaces:
		// Either Previous or Goroutine.
		if match := reRacePreviousOperationHeader.FindStringSubmatch(trimmed); match != nil {
			w := match[1] == "write"
			addr, err := strconv.ParseUint(match[2], 0, 64)
			if err != nil {
				return "", fmt.Errorf("failed to parse address on line: %q", strings.TrimSpace(trimmed))
			}
			id, err := strconv.Atoi(match[3])
			if err != nil {
				return "", fmt.Errorf("failed to parse goroutine id on line: %q", strings.TrimSpace(trimmed))
			}
			// Increase performance by always allocating 4 race operations minimally.
			if s.races == nil {
				s.races = make([]raceOp, 0, 4)
			}
			s.races = append(s.races, raceOp{w, addr, id})
			s.state = gotRaceOperationHeader
			return "", nil
		}
		if match := reRaceGoroutine.FindStringSubmatch(trimmed); match != nil {
			id, err := strconv.Atoi(match[1])
			if err != nil {
				return "", fmt.Errorf("failed to parse goroutine id on line: %q", strings.TrimSpace(trimmed))
			}
			g := &Goroutine{
				Signature: Signature{State: match[2]},
				ID:        id,
				First:     len(s.goroutines) == 0,
			}
			// Increase performance by always allocating 4 goroutines minimally.
			if s.goroutines == nil {
				s.goroutines = make([]*Goroutine, 0, 4)
			}
			s.goroutines = append(s.goroutines, g)
			s.state = gotRaceGoroutineHeader
			return "", nil
		}
		return "", fmt.Errorf("expected an operator or goroutine, got: %q", trimmed)

	default:
		return "", errors.New("internal error")
	}
}

// parseFunc only return an error if also returning a Call.
func parseFunc(c *Call, line string) (bool, error) {
	if match := reFunc.FindStringSubmatch(line); match != nil {
		c.Func.Raw = match[1]
		for _, a := range strings.Split(match[2], ", ") {
			if a == "..." {
				c.Args.Elided = true
				continue
			}
			if a == "" {
				// Remaining values were dropped.
				break
			}
			v, err := strconv.ParseUint(a, 0, 64)
			if err != nil {
				return true, fmt.Errorf("failed to parse int on line: %q", strings.TrimSpace(line))
			}
			// Increase performance by always allocating 4 values minimally.
			if c.Args.Values == nil {
				c.Args.Values = make([]Arg, 0, 4)
			}
			c.Args.Values = append(c.Args.Values, Arg{Value: v})
		}
		return true, nil
	}
	return false, nil
}

// parseFile only return an error if also processing a Call.
func parseFile(c *Call, line string) (bool, error) {
	if match := reFile.FindStringSubmatch(line); match != nil {
		num, err := strconv.Atoi(match[2])
		if err != nil {
			return true, fmt.Errorf("failed to parse int on line: %q", strings.TrimSpace(line))
		}
		c.SrcPath = match[1]
		c.Line = num
		return true, nil
	}
	return false, nil
}

// hasSrcPrefix returns true if any of s is the prefix of p.
func hasSrcPrefix(p string, s map[string]string) bool {
	for prefix := range s {
		if strings.HasPrefix(p, prefix+"/src/") || strings.HasPrefix(p, prefix+"/pkg/mod/") {
			return true
		}
	}
	return false
}

// getFiles returns all the source files deduped and ordered.
func getFiles(goroutines []*Goroutine) []string {
	files := map[string]struct{}{}
	for _, g := range goroutines {
		for _, c := range g.Stack.Calls {
			files[c.SrcPath] = struct{}{}
		}
	}
	if len(files) == 0 {
		return nil
	}
	out := make([]string, 0, len(files))
	for f := range files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// splitPath splits a path using "/" as separator into its components.
//
// The first item has its initial path separator kept.
func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	var out []string
	s := ""
	for _, c := range p {
		if c != '/' || (len(out) == 0 && strings.Count(s, "/") == len(s)) {
			s += string(c)
		} else if s != "" {
			out = append(out, s)
			s = ""
		}
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// isFile returns true if the path is a valid file.
func isFile(p string) bool {
	// TODO(maruel): Is it faster to open the file or to stat it? Worth a perf
	// test on Windows.
	i, err := os.Stat(p)
	return err == nil && !i.IsDir()
}

// rootedIn returns a root if the file split in parts is rooted in root.
//
// Uses "/" as path separator.
func rootedIn(root string, parts []string) string {
	//log.Printf("rootIn(%s, %v)", root, parts)
	for i := 1; i < len(parts); i++ {
		suffix := pathJoin(parts[i:]...)
		if isFile(pathJoin(root, suffix)) {
			return pathJoin(parts[:i]...)
		}
	}
	return ""
}

// findRoots sets member GOROOT and GOPATHs.
//
// This causes disk I/O as it checks for file presence.
func (c *Context) findRoots() {
	c.GOPATHs = map[string]string{}
	for _, f := range getFiles(c.Goroutines) {
		// TODO(maruel): Could a stack dump have mixed cases? I think it's
		// possible, need to confirm and handle.
		//log.Printf("  Analyzing %s", f)
		if c.GOROOT != "" && strings.HasPrefix(f, c.GOROOT+"/src/") {
			continue
		}
		if hasSrcPrefix(f, c.GOPATHs) {
			continue
		}
		parts := splitPath(f)
		if c.GOROOT == "" {
			if r := rootedIn(c.localgoroot+"/src", parts); r != "" {
				c.GOROOT = r[:len(r)-4]
				//log.Printf("Found GOROOT=%s", c.GOROOT)
				continue
			}
		}
		found := false
		for _, l := range c.localgopaths {
			if r := rootedIn(l+"/src", parts); r != "" {
				//log.Printf("Found GOPATH=%s", r[:len(r)-4])
				c.GOPATHs[r[:len(r)-4]] = l
				found = true
				break
			}
			if r := rootedIn(l+"/pkg/mod", parts); r != "" {
				//log.Printf("Found GOPATH=%s", r[:len(r)-8])
				c.GOPATHs[r[:len(r)-8]] = l
				found = true
				break
			}
		}
		if !found {
			// If the source is not found, just too bad.
			//log.Printf("Failed to find locally: %s", f)
		}
	}
}

// getGOPATHs returns parsed GOPATH or its default, using "/" as path separator.
func getGOPATHs() []string {
	var out []string
	if gp := os.Getenv("GOPATH"); gp != "" {
		for _, v := range filepath.SplitList(gp) {
			// Disallow non-absolute paths?
			if v != "" {
				v = strings.Replace(v, "\\", "/", -1)
				// Trim trailing "/".
				if l := len(v); v[l-1] == '/' {
					v = v[:l-1]
				}
				out = append(out, v)
			}
		}
	}
	if len(out) == 0 {
		homeDir := ""
		u, err := user.Current()
		if err != nil {
			homeDir = os.Getenv("HOME")
			if homeDir == "" {
				panic(fmt.Sprintf("Could not get current user or $HOME: %s\n", err.Error()))
			}
		} else {
			homeDir = u.HomeDir
		}
		out = []string{strings.Replace(homeDir+"/go", "\\", "/", -1)}
	}
	return out
}
