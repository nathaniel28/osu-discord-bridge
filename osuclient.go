package main

import (
	"bytes"
	"errors"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// tagged struct for reading part of the json osu sends over the websocket
type osuMessage struct {
	Content   string `json:"content"`
	ID        int    `json:"message_id"`
	ChannelID int    `json:"channel_id"`
	Action    bool   `json:"is_action"`
}

// ditto to comment on osuMessage
type osuUser struct {
	Name      string `json:"username"`
	AvatarURL string `json:"avatar_url"`
	ID        int    `json:"id"`
}

// all the parts of the json this program cares about
type messageEvent struct {
	Messages []osuMessage `json:"messages"`
	Users    []osuUser    `json:"users"`
}

type event struct {
	Err       string          `json:"error"`
	EventType string          `json:"event"`
	Data      json.RawMessage `json:"data"`
}

// what we pass to the OsuClient.Read channel
type Message struct {
	Content   string
	Author    string
	AvatarURL string
}

type OsuClient struct {
	http      http.Client
	websocket *websocket.Conn
	headers   http.Header // includes authorization details

	// watchOsu owns this request
	// cannot be used from any other thread
	keepalive *http.Request

	// used in watchOsu
	// cooldown used as is and as a start to exponential backoff
	cooldown     time.Duration // should be set once during init
	lastRecovery time.Time

	botUserID      int // essentially who to ignore
	watchChannelID int

	chatEndpoint string

	Read  chan Message // read messages from osu chat
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
		// TODO: probably define the time somewhere more obvious
		time.Sleep(90 * time.Second)
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

// can only be called by watchOsu; helper to watchOsu
var unreadyConnection = errors.New("ready event was not the first received")
func (c *OsuClient) initWebsocket() error {
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
	_, raw, err := c.websocket.ReadMessage()
	if err != nil {
		return err
	}
	var ev event
	err = json.Unmarshal(raw, &ev)
	if err != nil {
		return err
	}
	if ev.Err != "" {
		return fmt.Errorf("error over osu websocket %v", ev.Err)
	}
	if ev.EventType != "connection.ready" {
		return unreadyConnection
	}
	return nil
}

// helper to watchOsu
var tooManyRetries = errors.New("after trying pretty hard to recover, it didn't work")
func (c *OsuClient) tryRecovery() error {
	defer func() {
		c.lastRecovery = time.Now()
	}()
	elapsed := time.Since(c.lastRecovery)
	if c.cooldown > elapsed {
		time.Sleep(c.cooldown - elapsed)
	}
	err := c.initWebsocket()
	if err == nil {
		return nil
	}
	log.Printf("initial recovery failed: %s", err.Error())
	c.Read <- Message{
		Content: "osu reader failed, attempting to recover",
		Author: "(debug)",
	}
	wait := c.cooldown
	for i := 0; i < 12; i++ {
		if wait > 90 * time.Second {
			c.Read <- Message{
				Content: fmt.Sprintf("sleeping for %v before attempting recovery", wait),
				Author: "(debug)",
			}
		}
		time.Sleep(wait)
		err = c.initWebsocket()
		if err == nil {
			c.Read <- Message{
				Content: "recovered successfully",
				Author: "(debug)",
			}
			return nil
		}
		log.Println("failed recovery:", err.Error())
		wait *= 2
	}
	return tooManyRetries
}

func (c *OsuClient) watchOsu() {
	for i := 0; i < 4; i++ {
		err := c.initWebsocket()
		if err == nil {
			goto ready // sorry :P
		}
		log.Println("failed first websocket creation:", err.Error())
		time.Sleep(c.cooldown)
	}
	// TODO: fatal
	return
ready:
	cancelKeepalive := make(chan struct{})
	go c.keepaliveLoop(cancelKeepalive)
	var msg messageEvent
	var ev event
	log.Println("running watchOsu")
	for {
		_, raw, err := c.websocket.ReadMessage()
		if err != nil {
			cancelKeepalive <- struct{}{}
			if !errors.Is(err, net.ErrClosed) {
				err = c.tryRecovery()
				if err == nil {
					log.Println("recovered from websocket error:", err.Error())
					go c.keepaliveLoop(cancelKeepalive)
					continue
				}
				// TODO: that's fatal, report to main
			}
			return
		}

		err = json.Unmarshal(raw, &ev)
		if err != nil {
			log.Println("osu sent something strange and it could not be parsed:", err.Error())
			cancelKeepalive <- struct{}{}
			err = c.tryRecovery()
			if err == nil {
				continue
			}
			// TODO: fatal
			return
		}
		if ev.Err != "" {
			log.Println("error while reading osu chat:", ev.Err)
			cancelKeepalive <- struct{}{}
			err = c.tryRecovery()
			if err == nil {
				continue
			}
			// TODO: fatal
			return
		}
		switch ev.EventType {
		case "chat.message.new":
			err = json.Unmarshal(ev.Data, &msg)
			if err != nil {
				log.Println("could not parse as message:", err.Error())
				continue
			}
			lo := min(len(msg.Messages), len(msg.Users))
			for i := 0; i < lo; i++ {
				if msg.Users[i].ID == c.botUserID || msg.Messages[i].ChannelID != c.watchChannelID {
					continue
				}
				c.Read <- Message{
					Content: msg.Messages[i].Content,
					Author: msg.Users[i].Name,
					AvatarURL: msg.Users[i].AvatarURL,
				}
			}
		case "chat.channel.join":
			log.Println("joined channel??")
		case "chat.channel.part":
			log.Println("left channel??")
		default:
			log.Println("skipping unknown event type", ev.EventType)
		}
	}
	// dead code currently
	cancelKeepalive <- struct{}{}
}

func NewOsuClient(uid, chid int, authcode string, retryCooldown time.Duration) (*OsuClient, error) {
	client := OsuClient{
		headers: make(http.Header),
		botUserID: uid,
		watchChannelID: chid,
		chatEndpoint: fmt.Sprintf("https://osu.ppy.sh/api/v2/chat/channels/%v/messages", chid),
		cooldown: retryCooldown,
		Read: make(chan Message, 32),
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
	go c.writeLoop()
	go c.watchOsu()
	return nil
}

func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		case '"', '\\':
			b.WriteByte('\\')
			fallthrough
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *OsuClient) Send(msg string) {
	if len(c.Write) == cap(c.Write) {
		log.Println("dropping message, can't keep up")
		return
	}
	c.Write <- `{"message":"` + escape(msg) + `","is_action":false}`
}

func (c *OsuClient) Close() {
	close(c.Read)
	close(c.Write)
	c.websocket.Close()
}
