package operator

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

// AppendEvent appends one Event as an ndjson line to path.
func AppendEvent(path string, ev Event) error {
	if ev.TS == 0 {
		ev.TS = time.Now().Unix()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = f.Write(b)
	return err
}

// EmitEvent is a convenience that appends {ts,worker,event,detail} for a worker.
func (w Worker) EmitEvent(event string, detail interface{}) error {
	return AppendEvent(w.EventsPath(), Event{
		TS:     time.Now().Unix(),
		Worker: w.Name,
		Event:  event,
		Detail: detail,
	})
}

// ReadEvents parses all events from an ndjson file. Malformed lines are skipped.
func ReadEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}
