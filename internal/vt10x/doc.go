/*
Package terminal is a vt10x terminal emulation backend, influenced
largely by st, rxvt, xterm, and iTerm as reference. Use it for terminal
muxing, a terminal emulation frontend, or wherever else you need
terminal emulation.

In development, but very usable.

This is a vendored copy of github.com/hinshun/vt10x (v0.0.0-20220301184237)
with perch's changes:
  - WithScrollback: lines scrolling off the primary screen are handed out
  - ModePaste tracks bracketed paste (DECSET 2004)
  - CSI sequences with a <, =, > prefix (and ? outside modes and erases)
    are ignored instead of being misread as cursor restores or SGR resets
  - Write leaves a UTF-8 rune split across calls for the caller to resend
  - non-POSIX files removed
*/
package vt10x
