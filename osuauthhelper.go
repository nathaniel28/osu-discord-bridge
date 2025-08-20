package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

const (
	codeGetterEscaped = "http%3A%2F%2Flocalhost%3A7538" // must be escaped
	codeGetterAddr = "localhost:7538" // should match codeGetterEscaped

	// should true for when you don't use localhost
	// checks ./cert.pem and ./key.pem if true
	useTLS = false
)

type handler struct{
	server *http.Server
	code   chan string
	once   sync.Once
}

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(func() {
		h.code <- r.URL.Query().Get("code")
		h.server.Close()
	})
}

type token struct {
	Token   string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int    `json:"expires_in"`
}

type message struct {
	Content   string `json:"content"`
	ID        int    `json:"message_id"`
	ChannelID int    `json:"channel_id"`
	Action    bool   `json:"is_action"`
}

type user struct {
	Name string `json:"username"`
	ID   int    `json:"id"`
}

type messageEvent struct {
	Messages []message `json:"messages"`
	Users    []user    `json:"users"`
}

// thanks peppy for this nonsense I need to use to get the code
func osuAuthorize() (token token, err error) {
	fmt.Println("https://osu.ppy.sh/oauth/authorize?client_id=" + oauth2ID + "&redirect_uri=" + codeGetterEscaped + "&response_type=code&scope=chat.read+chat.write")

	server := http.Server{Addr: codeGetterAddr}
	handler := handler{
		server: &server,
		code: make(chan string, 1),
	}
	server.Handler = handler
	if useTLS {
		err = server.ListenAndServeTLS("cert.pem", "key.pem")
	} else {
		err = server.ListenAndServe()
	}
	if err != http.ErrServerClosed {
		return
	}
	code := <-handler.code
	close(handler.code)

	osuClient := http.Client{}
	body := bytes.Buffer{}
	body.WriteString("client_id=")
	body.WriteString(oauth2ID)
	body.WriteString("&client_secret=")
	body.WriteString(oauth2Secret)
	body.WriteString("&code=")
	body.WriteString(code)
	body.WriteString("&grant_type=authorization_code&redirect_uri=" + codeGetterEscaped)
	var getToken *http.Request
	getToken, err = http.NewRequest("POST", "https://osu.ppy.sh/oauth/token", &body)
	if err != nil {
		return
	}
	getToken.Header.Add("Accept", "application/json")
	getToken.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	var resp *http.Response
	resp, err = osuClient.Do(getToken)
	if err != nil {
		return
	}
	err = json.NewDecoder(resp.Body).Decode(&token)
	resp.Body.Close()
	return
}
