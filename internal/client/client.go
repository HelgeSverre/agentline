package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/securetoken"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Backoff time.Duration
}

type CreateResult struct {
	Room             model.Room        `json:"room"`
	Participant      model.Participant `json:"participant"`
	ParticipantToken string            `json:"participant_token"`
	InviteToken      string            `json:"invite_token"`
	InviteURL        string            `json:"invite_url"`
}

type ClaimResult struct {
	Room             model.Room        `json:"room"`
	Participant      model.Participant `json:"participant"`
	ParticipantToken string            `json:"participant_token"`
}

type WaitResult struct {
	Status      string         `json:"status"`
	Message     *model.Message `json:"message,omitempty"`
	Room        string         `json:"room,omitempty"`
	After       int64          `json:"after,omitempty"`
	Instruction string         `json:"instruction,omitempty"`
	EndedBy     string         `json:"ended_by,omitempty"`
	Sequence    int64          `json:"sequence,omitempty"`
}

type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string { return fmt.Sprintf("relay: %s: %s", e.Code, e.Message) }

func New(baseURL, token string, httpClient *http.Client) Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return Client{BaseURL: baseURL, Token: token, HTTP: httpClient, Backoff: 100 * time.Millisecond}
}

func ValidateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("invalid relay origin")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid relay origin")
		}
	}
	return nil
}

func (c Client) CreateRoom(ctx context.Context, name, creator string, ttl time.Duration) (CreateResult, error) {
	var out CreateResult
	err := c.do(ctx, http.MethodPost, "/v1/rooms", map[string]any{"name": name, "creator_name": creator, "ttl_seconds": ttl.Seconds()}, &out, false)
	if err == nil && strings.HasPrefix(out.InviteURL, "/") {
		out.InviteURL = c.BaseURL + out.InviteURL
	}
	return out, err
}

func InviteToken(invite string) (string, error) {
	u, err := url.Parse(invite)
	if err != nil || ValidateOrigin(u.Scheme+"://"+u.Host) != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("invalid invite URL")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 3 || parts[0] != "" || parts[1] != "join" || parts[2] == "" {
		return "", errors.New("invalid invite URL")
	}
	return parts[2], nil
}

func (c Client) ClaimInvite(ctx context.Context, invite, name string) (ClaimResult, error) {
	token, err := InviteToken(invite)
	if err != nil {
		return ClaimResult{}, err
	}
	u, _ := url.Parse(invite)
	base := c.BaseURL
	if u.IsAbs() {
		base = u.Scheme + "://" + u.Host
	}
	var out ClaimResult
	err = New(base, "", c.HTTP).do(ctx, http.MethodPost, "/v1/invites/"+url.PathEscape(token)+"/claim", map[string]string{"name": name}, &out, false)
	return out, err
}

func (c Client) Room(ctx context.Context, room string) (model.Room, error) {
	var out model.Room
	err := c.do(ctx, http.MethodGet, "/v1/rooms/"+url.PathEscape(room), nil, &out, true)
	return out, err
}

func (c Client) Send(ctx context.Context, room, id, body, replyTo string) (model.Message, error) {
	if id == "" {
		var err error
		id, err = securetoken.New(16)
		if err != nil {
			return model.Message{}, err
		}
	}
	var out model.Message
	err := c.do(ctx, http.MethodPost, "/v1/rooms/"+url.PathEscape(room)+"/messages", map[string]string{"id": id, "body": body, "reply_to": replyTo}, &out, true)
	return out, err
}

func (c Client) Read(ctx context.Context, room string, after int64) ([]model.Message, error) {
	var out struct {
		Messages []model.Message `json:"messages"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/rooms/"+url.PathEscape(room)+"/messages?after="+strconv.FormatInt(after, 10), nil, &out, true)
	return out.Messages, err
}

func (c Client) Wait(ctx context.Context, room string, after int64, timeout time.Duration) (WaitResult, error) {
	var out WaitResult
	path := "/v1/rooms/" + url.PathEscape(room) + "/wait?after=" + strconv.FormatInt(after, 10) + "&timeout=" + strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)
	err := c.do(ctx, http.MethodGet, path, nil, &out, false)
	return out, err
}

func (c Client) Done(ctx context.Context, room, id string) (model.Message, error) {
	if id == "" {
		var err error
		id, err = securetoken.New(16)
		if err != nil {
			return model.Message{}, err
		}
	}
	var out model.Message
	err := c.do(ctx, http.MethodPost, "/v1/rooms/"+url.PathEscape(room)+"/done", map[string]string{"id": id}, &out, true)
	return out, err
}

func (c Client) do(ctx context.Context, method, path string, input, output any, retry bool) error {
	if err := ValidateOrigin(c.BaseURL); err != nil {
		return err
	}
	var data []byte
	var err error
	if input != nil {
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		if input != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, err := c.HTTP.Do(req)
		transient := err != nil
		if err == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if output == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					return nil
				}
				err := json.NewDecoder(resp.Body).Decode(output)
				resp.Body.Close()
				return err
			}
			transient = resp.StatusCode >= 500
			if !transient || !retry || attempt == 2 {
				err := decodeError(resp)
				resp.Body.Close()
				return err
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if !retry || !transient || attempt == 2 {
			return fmt.Errorf("relay request failed: %w", err)
		}
		timer := time.NewTimer(c.Backoff * time.Duration(1<<attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func decodeError(resp *http.Response) error {
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || body.Error.Code == "" {
		return &Error{Status: resp.StatusCode, Code: "http_error", Message: http.StatusText(resp.StatusCode)}
	}
	return &Error{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
}
