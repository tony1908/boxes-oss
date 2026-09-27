package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type warmWindow struct {
	days       map[time.Weekday]bool
	start, end int
	location   *time.Location
	min        int
}

func parseWarmSchedule(value string) (*warmWindow, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	fields := strings.Fields(value)
	if len(fields) != 4 {
		return nil, fmt.Errorf("warmSchedule must be '<days> HH:MM-HH:MM <IANA time zone> min=<n>'")
	}
	days := map[string]time.Weekday{"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday, "Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday}
	selected := make(map[time.Weekday]bool)
	for _, part := range strings.Split(fields[0], ",") {
		rangeDays := strings.Split(part, "-")
		if len(rangeDays) > 2 {
			return nil, fmt.Errorf("invalid warmSchedule days")
		}
		first, ok := days[rangeDays[0]]
		if !ok {
			return nil, fmt.Errorf("invalid warmSchedule day %q", rangeDays[0])
		}
		last := first
		if len(rangeDays) == 2 {
			var valid bool
			last, valid = days[rangeDays[1]]
			if !valid {
				return nil, fmt.Errorf("invalid warmSchedule day %q", rangeDays[1])
			}
		}
		for d := first; ; d = (d + 1) % 7 {
			selected[d] = true
			if d == last {
				break
			}
		}
	}
	clock := strings.Split(fields[1], "-")
	if len(clock) != 2 {
		return nil, fmt.Errorf("invalid warmSchedule time range")
	}
	parseTime := func(raw string) (int, error) {
		t, e := time.Parse("15:04", raw)
		if e != nil || t.Format("15:04") != raw {
			return 0, fmt.Errorf("times must use 24-hour HH:MM format")
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	start, e := parseTime(clock[0])
	if e != nil {
		return nil, e
	}
	end, e := parseTime(clock[1])
	if e != nil {
		return nil, e
	}
	if start == end {
		return nil, fmt.Errorf("warmSchedule window must have a nonzero duration")
	}
	loc, e := time.LoadLocation(fields[2])
	if e != nil {
		return nil, fmt.Errorf("invalid IANA time zone")
	}
	if !strings.HasPrefix(fields[3], "min=") {
		return nil, fmt.Errorf("warmSchedule requires min=<n>")
	}
	min, e := parseNonnegative(fields[3])
	if e != nil {
		return nil, e
	}
	return &warmWindow{days: selected, start: start, end: end, location: loc, min: min}, nil
}

func effectivePoolMinimum(pool int, schedule string, now time.Time) int {
	w, err := parseWarmSchedule(schedule)
	if err != nil || w == nil {
		return pool
	}
	local := now.In(w.location)
	minute := local.Hour()*60 + local.Minute()
	active := w.days[local.Weekday()] && minute >= w.start && minute < w.end
	if w.end < w.start {
		active = active || (w.days[(local.Weekday()+6)%7] && minute < w.end)
	}
	if active && w.min > pool {
		return w.min
	}
	return pool
}
func parseNonnegative(value string) (int, error) {
	n, e := strconv.Atoi(strings.TrimPrefix(value, "min="))
	if e != nil || n < 0 {
		return 0, fmt.Errorf("warmSchedule min must be >= 0")
	}
	return n, nil
}
