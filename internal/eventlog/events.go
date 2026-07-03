package eventlog

import "os"

// TailCaptured returns up to maxLines lines from the end of events.jsonl (the
// capture log), or an empty string when the file doesn't exist yet.
func TailCaptured(maxLines int) (string, error) {
	return tailFile(EventsJSONLPath(), maxLines)
}

// tailFile reads up to maxLines lines from the end of path. Returns ("", nil)
// when the file is absent. Reads the whole file; intended for our rotated
// (bounded) logs, not arbitrary large files.
func tailFile(path string, maxLines int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if maxLines <= 0 {
		return string(data), nil
	}
	return lastLines(data, maxLines), nil
}

func lastLines(data []byte, n int) string {
	if len(data) == 0 || n <= 0 {
		return ""
	}
	count := 0
	i := len(data) - 1
	if data[i] == '\n' {
		i--
	}
	for ; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return string(data[i+1:])
			}
		}
	}
	return string(data)
}
