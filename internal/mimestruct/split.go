package mimestruct

import "bytes"

// span is a byte range of the raw message.
type span struct{ s, e int }

// split finds the parts of a multipart body raw[start:end] for the given
// boundary, following RFC 2046 section 5.1.1 as leniently as go-message's
// multipart reader does for well-formed mail:
//
//   - a delimiter is "--boundary" at the start of a line, followed by optional
//     white space and the end of the line (or "--" for the closing delimiter);
//   - the line break in front of a delimiter belongs to the delimiter, not to the
//     part before it;
//   - text before the first delimiter (preamble) and after the closing one
//     (epilogue) is ignored;
//   - a final part that is not closed runs to the end of the data.
//
// The work is bounded: the number of parts is capped at MaxParts (the builder
// gives up beyond it) and the scanned bytes are charged to the budget.
func (b *builder) split(start, end int, boundary string) []span {
	if boundary == "" || start >= end {
		return nil
	}
	b.charge(end - start)
	if b.over {
		return nil
	}
	delim := []byte("--" + boundary)
	raw := b.raw
	var spans []span
	partStart := -1
	crlf := true // the first delimiter line decides, as in go-message: LF-only mail has bare LF line breaks
	pos := start
	for pos < end {
		// Next "--boundary" that begins a line.
		i := -1
		for p := pos; p < end; {
			k := bytes.Index(raw[p:end], delim)
			if k < 0 {
				break
			}
			c := p + k
			if c == start || raw[c-1] == '\n' {
				i = c
				break
			}
			p = c + 1
		}
		if i < 0 {
			break
		}
		j := i + len(delim)

		// Closing delimiter: "--boundary--" and then only white space.
		closing := false
		rest := j
		if rest+1 < end && raw[rest] == '-' && raw[rest+1] == '-' {
			if lineIsBlank(raw, rest+2, end) {
				closing = true
			}
		}
		// Ordinary delimiter: only white space until the end of the line.
		lineEnd := -1
		if !closing {
			k := j
			for k < end && (raw[k] == ' ' || raw[k] == '\t') {
				k++
			}
			if k < end && raw[k] == '\r' {
				k++
			}
			if k < end && raw[k] == '\n' {
				lineEnd = k + 1
			}
		}
		if !closing && lineEnd < 0 {
			pos = i + 1 // "--boundaryXYZ" and the like: just text
			continue
		}

		if partStart >= 0 {
			e := i
			if e > partStart {
				e-- // the LF in front of the delimiter
				if e > partStart && raw[e-1] == '\r' {
					e--
				}
			}
			spans = append(spans, span{partStart, e})
			if len(spans) > MaxParts {
				b.over = true
				return nil
			}
		}
		if closing {
			return spans
		}
		if len(spans) == 0 && partStart < 0 {
			crlf = !(lineEnd-j >= 1 && raw[lineEnd-1] == '\n' && (lineEnd-2 < j || raw[lineEnd-2] != '\r'))
		}
		partStart = lineEnd
		pos = lineEnd
	}
	if partStart >= 0 {
		// Not closed: the last part runs to the end of the data, minus the final
		// line break (it could be the start of a delimiter that was cut off, which
		// go-message withholds as well).
		e := end
		if crlf {
			if e-partStart >= 2 && raw[e-2] == '\r' && raw[e-1] == '\n' {
				e -= 2
			}
		} else if e > partStart && raw[e-1] == '\n' {
			e--
		}
		spans = append(spans, span{partStart, e})
		if len(spans) > MaxParts {
			b.over = true
			return nil
		}
	}
	return spans
}

// lineIsBlank reports whether raw[from:end] up to the next line break (or the
// end) is only spaces and tabs.
func lineIsBlank(raw []byte, from, end int) bool {
	for k := from; k < end; k++ {
		switch raw[k] {
		case ' ', '\t':
		case '\r', '\n':
			return true
		default:
			return false
		}
	}
	return true
}
