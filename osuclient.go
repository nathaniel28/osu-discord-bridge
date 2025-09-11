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
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// will be deserialized from content received over the websocket connection
// tagged struct for reading part of the json osu sends over the websocket
type osuMessage struct {
	Content   string `json:"content"`
	ID        int    `json:"message_id"`
	ChannelID int    `json:"channel_id"`
	Action    bool   `json:"is_action"`
}

// will be deserialized from content received over the websocket connection
// ditto to comment on osuMessage
type osuUser struct {
	Name      string `json:"username"`
	AvatarURL string `json:"avatar_url"`
	ID        int    `json:"id"`
}

// will be deserialized from content received over the websocket connection
type messageEvent struct {
	Messages []osuMessage `json:"messages"`
	Users    []osuUser    `json:"users"`
}

// will be deserialized from content received over the websocket connection
type event struct {
	Err       string          `json:"error"`
	EventType string          `json:"event"`
	Data      json.RawMessage `json:"data"`
}

// acquired from <-OsuClient.Read
type Message struct {
	Content   string
	Author    string
	AvatarURL string
}

// send to OsuClient.Write <- to avoid ratelimiting
// send to OsuClient.WriteRated <- to include it
type Request struct {
	// the request you want sent; must be nonnil
	// don't set headers; they are set for you and yours will be overwritten
	Http    *http.Request

	// receipt may be nil (recall chans are reference types), but if it is
	// not, it must be a channel with a buffer of size 1 or greater; this is
	// to prevent readLoop from blocking
	Receipt chan error
}

type headerUpdate struct {
	headers http.Header
	version uint64
}

type headerRequest struct {
	// destination should never be closed
	destination chan headerUpdate
	version     uint64
}

type OsuClient struct {
	// NOTE: the following are safe for concurrent use

	http http.Client

	// a user of this struct should use <-OsuClient.Read to get chat updates
	// furthermore, users must never close any channel they have access to
	// however, users should be prepared for Read to close at any time
	// read as in past tense, not read
	// consider SendChat if your goal is to get a string posted in chat
	// to prevent race conditions WriteRated may only be closed by WriteLoop
	Read       chan Message
	Write      chan Request
	WriteRated chan Request

	// set headerRequest.version to 0 if you have not gotten any headers
	// yet, else set it to the version you most recently received
	// set headerRequest.destination to the channel (with a buffer size of
	// at least 1) you wish to receive your headerUpdate struct on
	// if you aquire headers this way, you must not modify them as the same
	// underlying map may be in use by multiple other threads
	updateHeaders chan headerRequest

	// used to prevent multiple Open()s or Close()s
	running atomic.Bool

	// NOTE: the following are unsafe for concurrent use

	// owned by readLoop
	ws *websocket.Conn // EXTRA_NOTE: ONLY ws.Close() is safe concurrently

	// owned by headerDispenser
	refresh    *http.Request
	accessTok  string
	refreshTok string

	// NOTE: the following are constants; they are not set after
	// NewOsuClient; therefore, they are safe for concurrent reads

	keepaliveReq *http.Request

	// starting point for exponential backoff, etc.
	cooldown time.Duration

	// botUserID is who to ignore when reading messages, since echo is not
	// wanted; watchChannelID is the osu channel ID to read from
	botUserID      int
	watchChannelID int

	// see NewOsuClient for details
	chatEndpoint string
}

func NewOsuClient(uid, chid int, access, refresh string, cool time.Duration) *OsuClient {
	keepalive, err := http.NewRequest("POST", "https://osu.ppy.sh/api/v2/chat/ack", nil)
	if err != nil {
		panic("NewOsuClient: failed to create keepalive *http.Request with http.NewRequest: " + err.Error())
	}
	refreshReq, err := http.NewRequest("POST", "https://osu.ppy.sh/oauth/token", nil)
	if err != nil {
		panic("NewOsuClient: failed to create refresh *http.Request with http.NewRequest: " + err.Error())
	}
	refreshReq.Header.Add("Accept", "application/json")
	refreshReq.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	return &OsuClient{
		Read: make(chan Message, 32),
		Write: make(chan Request, 32),
		WriteRated: make(chan Request, 32),
		updateHeaders: make(chan headerRequest, 4),
		refresh: refreshReq,
		accessTok: access,
		refreshTok: refresh,
		keepaliveReq: keepalive,
		cooldown: cool,
		botUserID: uid,
		watchChannelID: chid,
		chatEndpoint: fmt.Sprintf("https://osu.ppy.sh/api/v2/chat/channels/%v/messages", chid),
	}
}

// WARNING: Open is NOT SAFE FOR CONCURRENT USE
// This is because once open is called, the functions it starts as goroutines
// are allowed to call Close, and Open and Close are not allowed to be called
// concurrently
var alreadyOpen = errors.New("OsuClient was open before this attempt")
func (c *OsuClient) Open() error {
	if !c.running.CompareAndSwap(false, true) {
		return alreadyOpen
	}
	go c.headerDispenser()
	go c.writeLoop()
	go c.readLoop()
	return nil
}

// WARNING: While Close is safe for concurrent use with other calls to Close,
// it is NOT safe for concurrent use with calls to Open
// WARNING: Once and OsuClient is closed, it must not be re-opened; instead,
// create a new client with NewOsuClient
// TODO, perhaps: make a client re-openable
var alreadyClosed = errors.New("OsuClient was closed before this attempt")
func (c *OsuClient) Close() error {
	if !c.running.CompareAndSwap(true, false) {
		return alreadyClosed
	}
	c.ws.Close() // tells readLoop to shutdown
	c.Write <- Request{nil, nil} // tell writeLoop to shutdown
	c.updateHeaders <- headerRequest{nil, 0} // tell headerDispenser to shutdown
	return nil
}

func (c *OsuClient) SendChat(msg string) {
	// yes, check Write and not WriteRated...
	if len(c.Write) >= cap(c.Write) / 2 + cap(c.Write) / 4 {
		log.Println("dropping message, can't keep up")
		return
	}
	body := bytes.Buffer{}
	body.WriteString(`{"message":"`)
	body.WriteString(escape(msg))
	body.WriteString(`","is_action":false}`)
	req, err := http.NewRequest("POST", c.chatEndpoint, &body)
	if err != nil {
		// TODO: that's fatal
		log.Println("OsuClient.SendChat failed to construct request:", err)
		return
	}
	// ...but be sure to send on WriteRated!
	c.WriteRated <- Request{req, nil}
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

var noTokenFound = errors.New("access token or refresh token not found")
func (c *OsuClient) headerDispenser() {
	version := uint64(1)
	headers := make(http.Header)
	headers.Add("Authorization", "Bearer " + c.accessTok)
	headers.Add("Accept", "application/json")
	headers.Add("Content-Type", "application/json")
	for {
		hreq, ok := <-c.updateHeaders
		if !ok {
			log.Println("updateHeaders got closed, headerDispenser returning")
			return
		}
		if hreq.destination == nil {
			log.Println("shutting down headerDispenser")
			return
		}
		if hreq.version != version {
			hreq.destination <- headerUpdate{headers, version}
			continue
		}
		// they have the same version as us, and had a problem, so
		// we need new headers
		bbody := bytes.Buffer{}
		bbody.WriteString("client_id=")
		bbody.WriteString(oauth2ID)
		bbody.WriteString("&client_secret=")
		bbody.WriteString(oauth2Secret)
		bbody.WriteString("&grant_type=refresh_token&refresh_token=")
		bbody.WriteString(c.refreshTok)
		body := bytes.NewReader(bbody.Bytes())
		c.refresh.Body = io.NopCloser(body)
		c.refresh.ContentLength = int64(body.Len())
		// TODO: not setting c.refresh.GetBody, could this be a problem?
		err := exponentialBackoff(c.cooldown, 3 * time.Second, 5, func() (bool, error) {
			// the body may be reused, so rewind it
			body.Seek(0, io.SeekStart)
			// use c.http directly since writeLoop may be blocking
			// for this thread to give them new headers
			resp, err := c.http.Do(c.refresh)
			// TODO: for fatal errors, get better handling
			if err != nil {
				ohNo := "OsuClient.headerDispenser: unimplemented fatal error handling: failed to make refresh request: " + err.Error()
				log.Println(ohNo)
				panic(ohNo)
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				ohNo := fmt.Sprintf("OsuClient.headerDispenser: unimplemented fatal error handling: non 2xx status: %v", resp.StatusCode)
				log.Println(ohNo)
				panic(ohNo)
			}
			var tok token
			err = json.NewDecoder(resp.Body).Decode(&tok)
			if err != nil {
				return false, err
			}
			if len(tok.Access) == 0 || len(tok.Refresh) == 0 {
				return false, noTokenFound
			}

			// must copy headers, since it may be in use by other
			// threads
			// TODO: is http.Header.Clone suitable?
			replacement := make(http.Header)
			for k, v := range headers {
				replacement[k] = v
			}
			replacement.Set("Authorization", "Bearer " + tok.Access)
			headers = replacement
			version++
			c.accessTok = tok.Access
			c.refreshTok = tok.Refresh
			return false, nil
		})
		if err != nil {

			ohNo := "OsuClient.headerDispenser: unimplemented fatal error handling: failed to make refresh request: " + err.Error()
			log.Println(ohNo)
			panic(ohNo)
		}
		hreq.destination <- headerUpdate{headers, version}
	}
}

// don't call unless you're OsuClient.Open
// only one must be running at once
// NOTE: checking if certain channels are closed is a sanity check to prevent
// an evil fast-spinning loop of doom
// TODO: should somehow clear Write and WriteRated channels before shutting
// down, because users of this struct or maybe SendChat could be blocking on
// them
func (c *OsuClient) writeLoop() {
	r := headerRequest{make(chan headerUpdate, 1), 0}
	c.updateHeaders <- r
	h, ok := <-r.destination
	if !ok {
		return
	}

	// TODO: the messages/second is hardcoded, maybe change that?
	go func() {
		var sentThisCycle int
		var cycleEnd time.Time
		for {
			now := time.Now()
			if now.After(cycleEnd) {
				cycleEnd = now.Add(10 * time.Second)
				sentThisCycle = 1
			} else if sentThisCycle >= 25 {
				time.Sleep(cycleEnd.Sub(now))
			} else {
				sentThisCycle++
			}
			req, ok := <-c.WriteRated
			// sanity check
			if !ok {
				log.Println("WriteRated got closed, anonymous rated loop returning")
				return
			}
			// TODO: can I get a use of closed channel here during
			// a shutdown? better test it...
			// for now, check c.running too
			if !c.running.Load() {
				log.Println("checking c.running is required in ratelimited loop")
				return
			}
			c.Write <- req
		}
	}()
	// TODO: can't close channel because it may have writers
	// TODO: the above function needs to close when this function does
	// (currently it just will block forever)
	//defer close(c.WriteRated)

	for {
		req, ok := <-c.Write
		if !ok {
			log.Println("Write go closed, no longer listening to it, writeLoop returning")
			return
		}
		if req.Http == nil {
			log.Println("shutting down writeLoop")
			return
		}
	again:
		req.Http.Header = h.headers
		resp, err := c.http.Do(req.Http)
		if err != nil {
			if req.Receipt != nil {
				req.Receipt <- err
			}
			log.Println("bad request", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			if resp.StatusCode == 401 {
				r.version = h.version
				c.updateHeaders <- r
				h, ok = <-r.destination
				// sanity check
				if !ok {
					log.Println("but how?!")
					return
				}
				goto again // sorry :P
			} else {
				log.Println("something is my fault", resp)
			}
		}
		if req.Receipt != nil {
			req.Receipt <- nil
		}
	}
}

func (c *OsuClient) readLoop() {
	r := headerRequest{make(chan headerUpdate, 1), 0}
	c.updateHeaders <- r
	h, ok := <-r.destination
	if !ok {
		return
	}
	var err error
	c.ws, err = c.mkWebsocket(&h, &r)
	if err != nil {
		// TODO: fatal
		log.Println("fatal during websocket creation:", err)
		return
	}

	cancelKeepalive := make(chan struct{}, 1)
	go c.keepaliveLoop(cancelKeepalive)

	log.Println("started osu reader")
	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			cancelKeepalive <- struct{}{}
			if errors.Is(err, net.ErrClosed) {
				log.Println("shutting down readLoop")
				return
			}
			log.Println("readLoop websocket down:", err)
			c.ws.Close()
			c.ws, err = c.mkWebsocket(&h, &r)
			if err != nil {
				// TODO: fatal
				log.Println("fatal during websocket recovery:", err)
				return
			}
			go c.keepaliveLoop(cancelKeepalive)
			continue
		}
		var ev event
		err = json.Unmarshal(raw, &ev)
		if err != nil {
			log.Println("osu sent something that could not be parsed:", err)
			continue
		}
		if ev.Err != "" {
			log.Println("error while reading osu chat:", ev.Err)
			cancelKeepalive <- struct{}{}
			c.ws.Close()
			c.ws, err = c.mkWebsocket(&h, &r)
			if err != nil {
				// TODO: fatal
				log.Println("fatal during websocket recovery:", err)
				return
			}
			go c.keepaliveLoop(cancelKeepalive)
			continue
		}
		switch ev.EventType {
		case "chat.message.new":
			var msg messageEvent
			err = json.Unmarshal(ev.Data, &msg)
			if err != nil {
				log.Println("could not parse as message:", err)
				continue
			}
			lo := min(len(msg.Messages), len(msg.Users))
			for i := 0; i < lo; i++ {
				if msg.Users[i].ID == c.botUserID || msg.Messages[i].ChannelID != c.watchChannelID || len(msg.Messages[i].Content) == 0 {
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
}

var refreshNeeded = errors.New("current access token is outdated or otherwise invalid")
var unreadyConnection = errors.New("ready event was not the first received")
// mkWebsocket should only be called from readLoop
func (c *OsuClient) mkWebsocket(h *headerUpdate, r *headerRequest) (*websocket.Conn, error) {
	var goodws *websocket.Conn
	// TODO: lo is a hardcoded constant
	err := exponentialBackoff(c.cooldown, 3 * time.Second, 12, func() (bool, error) {
		ws, _, err := websocket.DefaultDialer.Dial("wss://notify.ppy.sh", h.headers)
		if err != nil {
			return false, err
		}
		err = ws.WriteMessage(websocket.TextMessage, []byte(`{"event":"chat.start"}`))
		if err != nil {
			ws.Close()
			return false, err
		}
		_, raw, err := ws.ReadMessage()
		if err != nil {
			ws.Close()
			return false, err
		}
		var ev event
		err = json.Unmarshal(raw, &ev)
		if err != nil {
			ws.Close()
			return false, err
		}
		if ev.Err != "" {
			ws.Close()
			// TODO: should it try getting new headers even if the
			// error is not authentication failed?
			if ev.Err == "authentication failed" {
				r.version = h.version
				c.updateHeaders <- *r
				var ok bool
				*h, ok = <-r.destination
				// sanity check
				if !ok {
					log.Println("actually how though")
					goodws = nil
					return false, nil
				}
				return true, refreshNeeded
			}
			return false, fmt.Errorf("error over osu websocket %v", ev.Err)
		}
		if ev.EventType != "connection.ready" {
			ws.Close()
			return false, unreadyConnection
		}
		goodws = ws
		return false, nil
	})
	if err == nil && goodws == nil {
		return nil, fmt.Errorf("failed to make a suitable websocket")
	}
	return goodws, err
}

// owned by readLoop, do not call elsewhere
func (c *OsuClient) keepaliveLoop(cancel chan struct{}) {
	// TODO: probably define the interval somewhere more obvious
	keepaliveInterval := 600 * time.Second
	doKeepalive := make(chan struct{}, 1)
	notify := func() {
		time.Sleep(keepaliveInterval)
		doKeepalive <- struct{}{}
	}
	var lastKeepalive time.Time // for runtime sanity checks
	go notify()
	for {
		select {
		case <-cancel:
			log.Println("shutting down keepaliveLoop")
			return
		case <-doKeepalive:
			now := time.Now()
			diff := now.Sub(lastKeepalive)
			t := keepaliveInterval - 30 * time.Second
			if diff < t {
				log.Println("dangerous issue: odd timing, multiple notifiers active?")
				// guard against somehow a second
				// go notify() in this thread
				time.Sleep(t - diff)
				lastKeepalive = time.Now()
			} else {
				lastKeepalive = now
			}
			// this gets to bypass the ratelimit
			c.Write <- Request{c.keepaliveReq, nil}
			go notify()
		}
	}
}

// if fn returned and error, it will try again (up to limit times)
// between tries, sleep for t seconds
// if fn returned true, sleep lo this cycle and do not increment the count
// if limit is reached, return the most recent error
// TODO: maybe make a nonblocking version? (if failed and sleep time is more
// than a second, start a goroutine to do it?)
func exponentialBackoff(t, lo time.Duration, limit int, fn func() (bool, error)) error {
	var err error
	j := 2 * limit
	for i := 0; i < limit; {
		var skip bool
		skip, err = fn()
		if err == nil {
			return nil
		}
		log.Println("backing off due to", err)
		if skip && j > 0 {
			j--
			log.Println("backoff sleeping shortly this time")
			time.Sleep(lo)
			continue
		}
		time.Sleep(t)
		t *= 2
		i++
	}
	return err
}
