// Copyright 2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Package report contains functions that process kernel output,
// detect/extract crash messages, symbolize them, etc.
package report

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/syzkaller/pkg/symbolizer"
)

type Reporter interface {
	// ContainsCrash searches kernel console output for oops messages.
	ContainsCrash(output []byte) bool

	// Parse extracts information about oops from console output.
	// Returns nil if no oops found.
	Parse(output []byte) *Report

	// Symbolize symbolizes rep.Report and fills in Maintainers.
	Symbolize(rep *Report) error
}

type Report struct {
	// Title contains a representative description of the first oops.
	Title string
	// Report contains whole oops text.
	Report []byte
	// Output contains whole raw console output as passed to Reporter.Parse.
	Output []byte
	// StartPos/EndPos denote region of output with oops message(s).
	StartPos int
	EndPos   int
	// Corrupted indicates whether the report is truncated of corrupted in some other way.
	Corrupted bool
	// Maintainers is list of maintainer emails.
	Maintainers []string
}

// NewReporter creates reporter for the specified OS:
// kernelSrc: path to kernel sources directory
// kernelObj: path to kernel build directory (can be empty for in-tree build)
// symbols: kernel symbols (result of pkg/symbolizer.ReadSymbols on kernel object file)
// ignores: optional list of regexps to ignore (must match first line of crash message)
func NewReporter(os, kernelSrc, kernelObj string, symbols map[string][]symbolizer.Symbol,
	ignores []*regexp.Regexp) (Reporter, error) {
	type fn func(string, string, map[string][]symbolizer.Symbol, []*regexp.Regexp) (Reporter, error)
	ctors := map[string]fn{
		"akaros":  ctorAkaros,
		"linux":   ctorLinux,
		"freebsd": ctorFreebsd,
		"netbsd":  ctorNetbsd,
		"fuchsia": ctorFuchsia,
		"windows": ctorWindows,
	}
	ctor := ctors[os]
	if ctor == nil {
		return nil, fmt.Errorf("unknown os: %v", os)
	}
	if kernelObj == "" {
		kernelObj = kernelSrc // assume in-tree build
	}
	return ctor(kernelSrc, kernelObj, symbols, ignores)
}

type oops struct {
	header       []byte
	formats      []oopsFormat
	suppressions []*regexp.Regexp
}

type oopsFormat struct {
	title        *regexp.Regexp
	report       *regexp.Regexp
	fmt          string
	noStackTrace bool
	corrupted    bool
}

func compile(re string) *regexp.Regexp {
	re = strings.Replace(re, "{{ADDR}}", "0x[0-9a-f]+", -1)
	re = strings.Replace(re, "{{PC}}", "\\[\\<[0-9a-f]+\\>\\]", -1)
	re = strings.Replace(re, "{{FUNC}}", "([a-zA-Z0-9_]+)(?:\\.|\\+)", -1)
	re = strings.Replace(re, "{{SRC}}", "([a-zA-Z0-9-_/.]+\\.[a-z]+:[0-9]+)", -1)
	return regexp.MustCompile(re)
}

func containsCrash(output []byte, oopses []*oops, ignores []*regexp.Regexp) bool {
	for pos := 0; pos < len(output); {
		next := bytes.IndexByte(output[pos:], '\n')
		if next != -1 {
			next += pos
		} else {
			next = len(output)
		}
		for _, oops := range oopses {
			match := matchOops(output[pos:next], oops, ignores)
			if match == -1 {
				continue
			}
			return true
		}
		pos = next + 1
	}
	return false
}

func matchOops(line []byte, oops *oops, ignores []*regexp.Regexp) int {
	match := bytes.Index(line, oops.header)
	if match == -1 {
		return -1
	}
	for _, supp := range oops.suppressions {
		if supp.Match(line) {
			return -1
		}
	}
	for _, ignore := range ignores {
		if ignore.Match(line) {
			return -1
		}
	}
	return match
}

func extractDescription(output []byte, oops *oops) (desc string, report []byte, format oopsFormat) {
	startPos := -1
	for _, f := range oops.formats {
		match := f.title.FindSubmatchIndex(output)
		if match == nil {
			continue
		}
		if startPos != -1 && startPos <= match[0] {
			continue
		}
		startPos = match[0]
		var args []interface{}
		for i := 2; i < len(match); i += 2 {
			args = append(args, string(output[match[i]:match[i+1]]))
		}
		desc = fmt.Sprintf(f.fmt, args...)
		report = output[startPos:]
		format = f
	}
	if len(desc) == 0 {
		pos := bytes.Index(output, oops.header)
		if pos == -1 {
			panic("non matching oops")
		}
		end := bytes.IndexByte(output[pos:], '\n')
		if end == -1 {
			end = len(output)
		} else {
			end += pos
		}
		desc = string(output[pos:end])
		report = output[pos:]
	}
	if len(desc) > 0 && desc[len(desc)-1] == '\r' {
		desc = desc[:len(desc)-1]
	}
	// Corrupted/intermixed lines can be very long.
	const maxDescLen = 180
	if len(desc) > maxDescLen {
		desc = desc[:maxDescLen]
	}
	return
}

// replace replaces [start:end] in where with what, inplace.
func replace(where []byte, start, end int, what []byte) []byte {
	if len(what) >= end-start {
		where = append(where, what[end-start:]...)
		copy(where[start+len(what):], where[end:])
		copy(where[start:], what)
	} else {
		copy(where[start+len(what):], where[end:])
		where = where[:len(where)-(end-start-len(what))]
		copy(where[start:], what)
	}
	return where
}
