package relay

import (
	"bytes"
	"html/template"
	"net/http"
	"time"

	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/web"
	"github.com/starfederation/datastar-go/datastar"
)

const inspectKeepalive = 25 * time.Second

type inspectPageData struct {
	Room      model.Room
	Messages  []model.Message
	EventsURL string
}

var inspectTemplates = template.Must(template.New("inspect").Funcs(template.FuncMap{
	"status": inspectStatus,
	"time":   inspectTime,
}).Parse(string(web.InspectHTML)))

func inspectStatus(status model.RoomStatus) string {
	if status == "done" {
		return "Ended"
	}
	return "Active"
}

func inspectTime(value time.Time) string {
	return value.Local().Format("Jan 2, 2006, 15:04 MST")
}

func (h *handler) inspectPage(w http.ResponseWriter, r *http.Request) {
	room, messages, ok := h.inspect(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	inspectHeaders(w)
	if err := inspectTemplates.ExecuteTemplate(w, "page", inspectPageData{
		Room: room, Messages: messages, EventsURL: "/inspect/" + r.PathValue("token") + "/events",
	}); err != nil {
		h.fail(w, err)
	}
}

func (h *handler) inspectCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(web.InspectCSS)
}

func (h *handler) inspectEvents(w http.ResponseWriter, r *http.Request) {
	room, messages, ok := h.inspect(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	inspectHeaders(w)
	stream := datastar.NewSSE(w, r)

	keepalive := time.NewTicker(inspectKeepalive)
	defer keepalive.Stop()
	seenSequence := int64(0)
	firstSnapshot := true

	for {
		updates, unsubscribe := h.subscribe(room.ID)
		updatedRoom, updatedMessages, err := h.store.Inspect(r.Context(), r.PathValue("token"))
		if err != nil {
			unsubscribe()
			return
		}
		if firstSnapshot {
			err = h.inspectSnapshot(stream, updatedRoom, updatedMessages)
			firstSnapshot = false
		} else {
			err = h.inspectChanges(stream, updatedRoom, updatedMessages, seenSequence)
		}
		if err != nil {
			unsubscribe()
			return
		}
		room, messages = updatedRoom, updatedMessages
		seenSequence = lastSequence(messages)
		if room.Status == "done" {
			unsubscribe()
			return
		}
		select {
		case <-updates:
			unsubscribe()
		case <-keepalive.C:
			unsubscribe()
			if err := stream.Send(datastar.EventTypePatchSignals, []string{"signals {}"}); err != nil {
				return
			}
		case <-r.Context().Done():
			unsubscribe()
			return
		}
	}
}

func (h *handler) inspect(w http.ResponseWriter, r *http.Request) (model.Room, []model.Message, bool) {
	room, messages, err := h.store.Inspect(r.Context(), r.PathValue("token"))
	if err != nil {
		h.fail(w, err)
		return model.Room{}, nil, false
	}
	return room, messages, true
}

func inspectHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Robots-Tag", "noindex, noarchive")
}

func (h *handler) inspectSnapshot(stream *datastar.ServerSentEventGenerator, room model.Room, messages []model.Message) error {
	facts, err := inspectHTML("facts", room)
	if err != nil {
		return err
	}
	transcript, err := inspectHTML("messages", messages)
	if err != nil {
		return err
	}
	if err := writeDatastarPatch(stream, "#room-facts", datastar.ElementPatchModeOuter, facts); err != nil {
		return err
	}
	return writeDatastarPatch(stream, "#messages-area", datastar.ElementPatchModeOuter, transcript)
}

func (h *handler) inspectChanges(stream *datastar.ServerSentEventGenerator, room model.Room, messages []model.Message, after int64) error {
	for _, message := range messages {
		if message.Sequence <= after {
			continue
		}
		html, err := inspectHTML("message", message)
		if err != nil {
			return err
		}
		if err := writeDatastarPatch(stream, "#messages", datastar.ElementPatchModeAppend, html); err != nil {
			return err
		}
	}
	if room.Status == "done" {
		facts, err := inspectHTML("facts", room)
		if err != nil {
			return err
		}
		return writeDatastarPatch(stream, "#room-facts", datastar.ElementPatchModeOuter, facts)
	}
	return nil
}

func inspectHTML(name string, value any) (string, error) {
	var out bytes.Buffer
	if err := inspectTemplates.ExecuteTemplate(&out, name, value); err != nil {
		return "", err
	}
	return out.String(), nil
}

func writeDatastarPatch(stream *datastar.ServerSentEventGenerator, selector string, mode datastar.ElementPatchMode, html string) error {
	return stream.PatchElements(html, datastar.WithSelector(selector), datastar.WithMode(mode))
}

func lastSequence(messages []model.Message) int64 {
	if len(messages) == 0 {
		return 0
	}
	return messages[len(messages)-1].Sequence
}
