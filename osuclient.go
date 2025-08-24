package main

import (
	"bytes"
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type OsuClient struct {
	http      http.Client
	websocket *websocket.Conn
	headers   http.Header // includes authorization details

	// watchOsu owns this request
	// cannot be used from any other thread
	keepalive *http.Request

	botUserID      int // essentially who to ignore
	watchChannelID int

	chatEndpoint string

	Read  chan string // read messages from osu chat
	Write chan string // consider Send() instead; this is not for messages
}

// NOTE: osu! says 10 messages/5 seconds for pms, #multiplayer, and #spectator,
// but we are active in none of those, so without knowledge of the real rate
// limit I've settled on 25 messages/10 seconds and hopefully that's close...
// NOTE: Only two parts make continuous requests to the osu!api, this one and
// the keepalive. This part is the only one partially controlled by users, so
// I'm only rate limiting this one (why would you rate limit a keepalive anyway)
func (c *OsuClient) writeLoop() {
	var sentThisCycle int
	var cycleEnd time.Time
	for {
		// yes, before actually reading c.Write
		// it's less accurate but the channel can act as a buffer
		// with no extra support
		now := time.Now()
		if now.After(cycleEnd) {
			cycleEnd = now.Add(10 * time.Second)
			sentThisCycle = 1
		} else if sentThisCycle >= 25 {
			time.Sleep(cycleEnd.Sub(now))
		} else {
			sentThisCycle++
		}
		msg, ok := <-c.Write
		if !ok {
			return
		}
		body := bytes.Buffer{}
		body.WriteString(msg)
		req, err := http.NewRequest("POST", c.chatEndpoint, &body)
		if err != nil {
			// TODO: that's fatal
			continue
		}
		req.Header = c.headers
		resp, err := c.http.Do(req)
		if err != nil {
			log.Println("send message request:", err.Error())
			continue
		}
		resp.Body.Close()
	}
}

// managed by watchOsu; do not call elsewhere
func (c *OsuClient) keepaliveLoop(cancel chan struct{}) {
	notify := func(ch chan struct{}) {
		time.Sleep(30 * time.Second)
		ch <- struct{}{}
	}
	nchan := make(chan struct{}, 1)
	go notify(nchan)
	for {
		select {
		case <-cancel:
			return
		case <-nchan:
			go notify(nchan)
			resp, err := c.http.Do(c.keepalive)
			if err != nil {
				log.Println("dokeepalive:", err.Error())
				continue
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				log.Println("dokeepalive odd status:", resp)
			}
			resp.Body.Close()
		}
	}
}

func (c *OsuClient) watchOsu() {
	cancelKeepalive := make(chan struct{}, 1)
	go c.keepaliveLoop(cancelKeepalive)
	var msg messageEvent
	var lastRecovery time.Time
mainLoop:
	for {
		_, raw, err := c.websocket.ReadMessage()
		if err != nil {
			cancelKeepalive <- struct{}{}
			if !errors.Is(err, net.ErrClosed) {
				now := time.Now()
				diff := now.Sub(lastRecovery)
				// TODO: create constant for recovery cooldown
				if diff < 30 * time.Second {
					time.Sleep(diff)
				}
				err2 := c.recoverWebsocket()
				if err2 == nil {
					log.Println("recovered from websocket error:", err.Error())
					lastRecovery = time.Now()
					go c.keepaliveLoop(cancelKeepalive)
					continue
				}
				log.Printf("osu reader failed: %s; initial recovery failed: %s", err.Error(), err2.Error())
				c.Read <- "osu reader failed, attempting to recover"
				wait := 10 * time.Second
				for i := 0; i < 12; i++ {
					if wait > 1 * time.Minute {
						c.Read <- fmt.Sprintf("sleeping for %v before attempting recovery", wait)
					}
					time.Sleep(wait)
					err = c.recoverWebsocket()
					if err == nil {
						lastRecovery = time.Now()
						c.Read <- "recovered successfully"
						go c.keepaliveLoop(cancelKeepalive)
						continue mainLoop
					}
					log.Printf("failed recovery: %v", err.Error())
					wait *= 2
				}
				// TODO: that's fatal, report to main
			}
			return
		}
		dec := json.NewDecoder(bytes.NewBuffer(raw))
		if !expect[json.Delim](dec, '{') {
			log.Println("not json object: ")
			logRemainder(dec)
			continue
		}
		// NOTE: we rely on the event field being specified first
		fieldName, ok := expectAny[string](dec)
		if !ok || fieldName != "event" {
			log.Printf("got unexpected json:\n{\"%s\"", fieldName)
			logRemainder(dec)
			continue
		}
		evType, ok := expectAny[string](dec)
		if !ok {
			log.Println("very odd json: ")
			logRemainder(dec)
			continue
		}
		if !expect[string](dec, "data") {
			log.Printf("skipping dataless %s\n", evType)
			continue
		}
		switch evType {
		case "chat.message.new":
			err = dec.Decode(&msg)
			if err != nil {
				log.Println(err)
				continue
			}
			lo := min(len(msg.Messages), len(msg.Users))
			for i := 0; i < lo; i++ {
				if msg.Users[i].ID == c.botUserID || msg.Messages[i].ChannelID != c.watchChannelID {
					continue
				}
				c.Read <- fmt.Sprintf("%s: %s", msg.Users[i].Name, msg.Messages[i].Content)
			}
		default:
			log.Printf("skipping %s\n", evType)
		}
	}
	// dead code currently
	cancelKeepalive <- struct{}{}
}

// can only be called when watchOsu is not running
// or you have concurrent writes to the *websocket.Conn
// NOTE: make a lock if writes happen from somewhere else
func (c *OsuClient) recoverWebsocket() error {
	if c.websocket != nil {
		c.websocket.Close()
	}
	var err error
	c.websocket, _, err = websocket.DefaultDialer.Dial("wss://notify.ppy.sh", c.headers)
	if err != nil {
		return err
	}
	err = c.websocket.WriteMessage(websocket.TextMessage, []byte(`{"event":"chat.start"}`))
	if err != nil {
		return err
	}
	return nil
}

func NewOsuClient(uid, chid int, authcode string) (*OsuClient, error) {
	client := OsuClient{
		headers: make(http.Header),
		botUserID: uid,
		watchChannelID: chid,
		chatEndpoint: fmt.Sprintf("https://osu.ppy.sh/api/v2/chat/channels/%v/messages", chid),
		Read: make(chan string, 32),
		Write: make(chan string, 32),
	}

	var err error
	client.keepalive, err = http.NewRequest("POST", "https://osu.ppy.sh/api/v2/chat/ack", nil)
	if err != nil {
		return nil, err
	}
	client.headers.Add("Authorization", "Bearer " + authcode)
	client.headers.Add("Accept", "application/json")
	client.headers.Add("Content-Type", "application/json")
	client.keepalive.Header = client.headers

	return &client, nil
}

func (c *OsuClient) Open() error {
	if err := c.recoverWebsocket(); err != nil {
		return err
	}
	go c.writeLoop()
	go c.watchOsu()
	return nil
}

func (c *OsuClient) Send(msg string) {
	if len(c.Write) == cap(c.Write) {
		log.Println("dropping message, can't keep up")
		return
	}
	c.Write <- `{"message":"` + msg + `","is_action":false}`
}

func (c *OsuClient) Close() {
	close(c.Read)
	close(c.Write)
	c.websocket.Close()
}

func expect[T comparable](dec *json.Decoder, t T) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	c, ok := tok.(T)
	return ok && c == t
}

func expectAny[T any](dec *json.Decoder) (T, bool) {
	tok, err := dec.Token()
	if err != nil {
		var zero T
		return zero, false
	}
	c, ok := tok.(T)
	return c, ok
}

func logRemainder(dec *json.Decoder) {
	// hehe
	io.Copy(log.Default().Writer(), io.MultiReader(dec.Buffered(), bytes.NewReader([]byte{'\n'})))
}
