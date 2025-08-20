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

// TODO, perhaps
/*
// I though about having one channel for both messages and errors, but
// defining a struct to contain a string and an error or using anys seemed worse
type Messenger interface {
	// Deliver a message. Send must not block.
	// Must only be used from one thread.
	Send(string)

	// Block until a message is ready or an error is had
	ReceiveChan() <-chan string

	// Detect fatal errors
	ErrorChan() <-chan error

	// Close the connection to the remote resource
	Close() error
}
*/

type OsuClient struct {
	http      http.Client
	websocket *websocket.Conn
	headers   http.Header // includes authorization details

	keepalive *http.Request

	stopKeepalive chan struct{} // stop keepaliveLoop()

	botUserID      int // essentially who to ignore
	watchChannelID int

	chatEndpoint string

	Read  chan string // read messages from osu chat
	Write chan string // consider Send() instead; this is not for messages

	// TODO: lock these if more recover* style functions are added
	lastRecovery time.Time
	soonestRetry time.Duration // how we know if recoveries happen too often
}

func (c *OsuClient) keepaliveLoop() {
	notify := func(ch chan struct{}) {
		time.Sleep(30 * time.Second)
		ch <- struct{}{}
	}
	nchan := make(chan struct{}, 1)
	go notify(nchan)
	for {
		select {
		case <-c.stopKeepalive:
			return
		case <-nchan:
			go notify(nchan)
			resp, err := c.http.Do(c.keepalive)
			if err != nil {
				log.Println("dokeepalive:", err)
				return
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				log.Println("dokeepalive odd status:", resp)
			}
			resp.Body.Close()
		}
	}
}

func (c *OsuClient) writeLoop() {
	for {
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
			log.Println("send message request:", err)
			continue
		}
		resp.Body.Close()
	}
}

func (c *OsuClient) watchOsu() {
	var msg messageEvent
	for {
		_, raw, err := c.websocket.ReadMessage()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				if err = c.recoverWebsocket(); err == nil {
					log.Println("recovered from websocket error")
					continue
				}
				// TODO: that's fatal
			}
			return
		}
		dec := json.NewDecoder(bytes.NewBuffer(raw))
		if !(expect[json.Delim](dec, '{') &&
			expect[string](dec, "event")) {

			log.Println("odd json: ")
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
}

var TooQuickRecoveries = errors.New("recovories occurred at a rapid interval")
func (c *OsuClient) recoverWebsocket() error {
	now := time.Now()
	if now.Sub(c.lastRecovery) < c.soonestRetry {
		return TooQuickRecoveries
	}
	c.lastRecovery = now

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

func NewOsuClient(uid, chid int, authcode string, soonestRecoveryRetry time.Duration) (*OsuClient, error) {
	client := OsuClient{
		headers: make(http.Header),
		botUserID: uid,
		watchChannelID: chid,
		chatEndpoint: fmt.Sprintf("https://osu.ppy.sh/api/v2/chat/channels/%v/messages", chid),
		Read: make(chan string, 32),
		Write: make(chan string, 32),
		stopKeepalive: make(chan struct{}, 1),
		soonestRetry: soonestRecoveryRetry,
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
	//close(c.Read)
	//close(c.Write)
	c.websocket.Close()
	c.stopKeepalive <- struct{}{}
	//close(c.stopKeepalive)
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
