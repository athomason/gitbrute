/*
Copyright 2014 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The gitbrute command brute-forces a git commit hash of specific forms.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	mode    = flag.String("mode", "prefix", "Match determination method: prefix, suffix, regex, bingo, numeric, ascending")
	prefix  = flag.String("prefix", "abcdef", "Desired prefix or suffix")
	regex   = flag.String("regex", "", "Regular expression to match, if provided; supercedes prefix")
	force   = flag.Bool("force", false, "Re-run, even if current hash matches")
	pretend = flag.Bool("pretend", false, "Don't amend, just output winning hash")
	cpu     = flag.Int("cpus", runtime.NumCPU(), "Number of CPUs to use. Defaults to number of processors.")
	profile = flag.String("profile", "", "pprof file")
)

var (
	start     = time.Now()
	startUnix = start.Unix()
	rx        *regexp.Regexp
)

func main() {
	flag.Parse()
	runtime.GOMAXPROCS(*cpu)

	switch *mode {
	case "prefix", "suffix":
		if _, err := strconv.ParseInt(*prefix, 16, 64); err != nil {
			log.Fatalf("Prefix %q isn't hex.", *prefix)
		}
	case "regex":
		rx = regexp.MustCompile(*regex)
	case "bingo", "numeric", "ascending":
	default:
		log.Fatalf("Unknown mode %s", *mode)
	}

	hash := curHash()
	rawHash, _ := hex.DecodeString(hash)
	if !*force {
		switch *mode {
		case "prefix":
			if strings.HasPrefix(hash, *prefix) {
				return
			}
		case "suffix":
			if strings.HasSuffix(hash, *prefix) {
				return
			}
		case "regex":
			if rx.MatchString(hash) {
				return
			}
		case "bingo":
			if isBingo(rawHash) {
				return
			}
		case "numeric":
			if isNumeric(rawHash) {
				return
			}
		case "ascending":
			if isAscending(rawHash) {
				return
			}
		}
	}

	obj, err := exec.Command("git", "cat-file", "-p", hash).Output()
	if err != nil {
		log.Fatal(err)
	}
	i := bytes.Index(obj, []byte("\n\n"))
	if i < 0 {
		log.Fatalf("No \\n\\n found in %q", obj)
	}
	msg := obj[i+2:]

	if *profile != "" {
		fh, err := os.Create(*profile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(fh)
		defer pprof.StopCPUProfile()
	}

	winner := make(chan solution)
	var done uint32

	for i := 0; i < *cpu; i++ {
		go bruteForce(obj, winner, *cpu, i, &done)
	}

	w := <-winner
	atomic.StoreUint32(&done, 1)

	if *pretend {
		return
	}

	cmd := exec.Command("git", "commit", "--amend", "--date="+w.author.String(), "--file=-")
	cmd.Env = append([]string{"GIT_COMMITTER_DATE=" + w.committer.String()}, os.Environ()...)
	cmd.Stdout = os.Stdout
	cmd.Stdin = bytes.NewReader(msg)
	if err := cmd.Run(); err != nil {
		log.Fatalf("amend: %v", err)
	}
}

type solution struct {
	author, committer date
}

var (
	authorDateRx   = regexp.MustCompile(`(?m)^author.+> (.+)`)
	commiterDateRx = regexp.MustCompile(`(?m)^committer.+> (.+)`)
)

func bruteForce(obj []byte, winner chan<- solution, period, offset int, done *uint32) {
	e := newExplorer(period, offset)

	// blob is the blob to mutate in-place repatedly while testing
	// whether we have a match.
	blob := []byte(fmt.Sprintf("commit %d\x00%s", len(obj), obj))
	authorDate, adatei := getDate(blob, authorDateRx)
	commitDate, cdatei := getDate(blob, commiterDateRx)

	s1 := sha1.New()
	wantHexExact := []byte(*prefix)
	hexBuf := make([]byte, 0, sha1.Size*2)

	var match func([]byte) bool
	switch *mode {
	case "prefix":
		match = func(h []byte) bool {
			return bytes.HasPrefix(hexInPlace(h), wantHexExact)
		}
	case "suffix":
		match = func(h []byte) bool {
			return bytes.HasSuffix(hexInPlace(h), wantHexExact)
		}
	case "regex":
		match = func(h []byte) bool {
			return rx.Match(hexInPlace(h))
		}
	case "bingo":
		match = isBingo
	case "numeric":
		match = isNumeric
	case "ascending":
		match = isAscending
	}
	for atomic.LoadUint32(done) == 0 {
		t := e.next()
		ad := date{startUnix - int64(t.authorBehind), authorDate.tz}
		cd := date{startUnix - int64(t.commitBehind), commitDate.tz}
		strconv.AppendInt(blob[:adatei], ad.n, 10)
		strconv.AppendInt(blob[:cdatei], cd.n, 10)
		s1.Reset()
		s1.Write(blob)
		h := s1.Sum(hexBuf[:0])
		if !match(h) {
			continue
		}

		if *pretend {
			fmt.Printf("%s (%s, %s)\n", hexInPlace(h), ad, cd)
		}

		winner <- solution{ad, cd}
		return
	}
}

// try is a pair of seconds behind now to brute force, looking for a
// matching commit.
type try struct {
	commitBehind int
	authorBehind int
}

type explorer struct {
	period, m, n int
}

// explorer.next() yields the sequence:
//	   (0, 0)
//
//	   (0, 1)
//	   (1, 0)
//
//	   (0, 2)
//	   (1, 1)
//	   (2, 0)
//
//	   (0, 3)
//	   (1, 2)
//	   (2, 1)
//	   (3, 0)
//
//	   (0, 4)
//	   (1, 3)
//	   (2, 2)
//	   (3, 1)
//	   (4, 0)
//
//     ...
//
// except only every period'th element is emitted, starting from the
// offset'dth.
func newExplorer(period, offset int) *explorer {
	e := explorer{period: period}
	e.advance(offset)
	return &e
}

func (e *explorer) advance(by int) {
	for i := 0; i < by; i++ {
		e.n++
		if e.n > e.m {
			e.n = 0
			e.m++
		}
	}
}

func (e *explorer) next() try {
	t := try{e.n, e.m - e.n}
	e.advance(e.period)
	return t
}

// date is a git date.
type date struct {
	n  int64 // unix seconds
	tz string
}

func (d date) String() string { return fmt.Sprintf("%d %s", d.n, d.tz) }

// getDate parses out a date from a git header (or blob with a header
// following the size and null byte). It returns the date and index
// that the unix seconds begins at within h.
func getDate(h []byte, rx *regexp.Regexp) (d date, idx int) {
	m := rx.FindSubmatchIndex(h)
	if m == nil {
		log.Fatalf("Failed to match %s in %q", rx, h)
	}
	v := string(h[m[2]:m[3]])
	space := strings.Index(v, " ")
	if space < 0 {
		log.Fatalf("unexpected date %q", v)
	}
	n, err := strconv.ParseInt(v[:space], 10, 64)
	if err != nil {
		log.Fatalf("unexpected date %q", v)
	}
	return date{n, v[space+1:]}, m[2]
}

func curHash() string {
	all, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		log.Fatal(err)
	}
	h := string(all)
	if i := strings.Index(h, "\n"); i > 0 {
		h = h[:i]
	}
	return h
}

// hexInPlace takes a slice of binary data and returns the same slice with double
// its length, hex-ified in-place.
func hexInPlace(v []byte) []byte {
	const hex = "0123456789abcdef"
	h := v[:len(v)*2]
	for i := len(v) - 1; i >= 0; i-- {
		b := v[i]
		h[i*2+0] = hex[b>>4]
		h[i*2+1] = hex[b&0xf]
	}
	return h
}

// isBingo is a fast path for /^[a-f]{7}/
func isBingo(h []byte) bool {
	for _, b := range h[:3] {
		if b>>4 < 0xa || b&0xf < 0xa {
			return false
		}
	}
	if h[3]>>4 < 0xa {
		return false
	}
	return true
}

// isNumeric is a fast path for /^[0-9]{40}$/
func isNumeric(h []byte) bool {
	for _, b := range h {
		if b>>4 >= 0xa || b&0xf >= 0xa {
			return false
		}
	}
	return true
}

// isAscending returns true if the leading hex digits are non-decreasing
func isAscending(h []byte) bool {
	const nibs = 12
	const hn = nibs / 2

	var hlast byte
	for i, b := range h[:hn] {
		h1 := b >> 4
		if i > 0 && hlast > h1 {
			return false
		}
		h2 := b & 0xf
		if h1 > h2 {
			return false
		}
		hlast = h2
	}
	if nibs%2 != 0 {
		if hlast > h[hn]>>4 {
			return false
		}
	}

	return true
}
