// Package markedblock replaces generated blocks while preserving other bytes
package markedblock

import (
	"bytes"
	"fmt"
)

// Replace moves a generated block to the end of the document. Markers must
// occupy their own lines; marker text inside another line remains untouched
// An empty body removes the block
func Replace(current []byte, begin, end string, body []byte) ([]byte, error) {
	start, stop, offset := -1, -1, 0
	for line := range bytes.SplitAfterSeq(current, []byte("\n")) {
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		switch string(content) {
		case begin:
			if start >= 0 || stop >= 0 {
				return nil, fmt.Errorf("incomplete or duplicated %s block", begin)
			}
			start = offset
		case end:
			if start < 0 || stop >= 0 {
				return nil, fmt.Errorf("incomplete or duplicated %s block", begin)
			}
			stop = offset + len(line)
		default:
			if bytes.HasPrefix(content, []byte(begin)) || bytes.HasPrefix(content, []byte(end)) {
				return nil, fmt.Errorf("%s markers must occupy their own lines", begin)
			}
		}
		offset += len(line)
	}
	if start >= 0 && stop < 0 {
		return nil, fmt.Errorf("incomplete or duplicated %s block", begin)
	}
	if start < 0 && len(body) == 0 {
		return current, nil
	}

	before := current
	var after []byte
	if start >= 0 {
		before, after = current[:start], current[stop:]
	}
	extra := 0
	last := after
	if len(last) == 0 {
		last = before
	}
	needsNewline := len(body) > 0 && len(last) > 0 && last[len(last)-1] != '\n'
	if len(body) > 0 {
		extra = len(begin) + len(body) + len(end) + 2
		if needsNewline {
			extra++
		}
	}
	result := make([]byte, 0, len(before)+len(after)+extra)
	result = append(result, before...)
	result = append(result, after...)
	if len(body) == 0 {
		return result, nil
	}
	if needsNewline {
		result = append(result, '\n')
	}
	result = append(result, begin...)
	result = append(result, '\n')
	result = append(result, body...)
	result = append(result, end...)
	return append(result, '\n'), nil
}
