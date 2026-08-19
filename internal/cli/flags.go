package cli

import (
	"strconv"
	"strings"
	"time"
)

type durationValue time.Duration

func (d *durationValue) String() string { return time.Duration(*d).String() }
func (d *durationValue) Type() string   { return "duration" }
func (d *durationValue) Set(value string) error {
	var duration time.Duration
	var err error
	if strings.HasSuffix(value, "d") && !strings.Contains(value[:len(value)-1], "d") {
		var days float64
		days, err = strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		duration = time.Duration(days * float64(24*time.Hour))
	} else {
		duration, err = time.ParseDuration(value)
	}
	if err != nil {
		return err
	}
	*d = durationValue(duration)
	return nil
}

type createOpts struct {
	name            string
	roomName        string
	server          string
	local           bool
	ttl             durationValue
	maxParticipants int
}

type joinOpts struct {
	name string
}

type sendOpts struct {
	replyTo   string
	messageID string
	to        string
}

type readWaitOpts struct {
	after   int64
	timeout time.Duration
}

type doneOpts struct {
	messageID string
}

type channelOpts struct {
	room string
}

type serverOpts struct {
	listen        string
	publicURL     string
	data          string
	localInstance string
}

type setupOpts struct {
	yes    bool
	native bool
	remove bool
}

type doctorOpts struct {
	target string
	server string
}
