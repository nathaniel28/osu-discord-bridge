package main

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	osuBotID = 36497341
	//osuWatchChannel = 58051697 // priv
	osuWatchChannel = 58277602 // HML
	//discordWatchChannel = "1130978005448142879" // priv
	discordWatchChannel = "1407449989969477742" // HML:relay

	logPath = "./log"
	logSize = 1048576
)

//go:embed oauth2id
var oauth2ID string
//go:embed oauth2secret
var oauth2Secret string
//go:embed discordtoken
var discordToken string

func main() {
	var rlog CircularLog
	if err := rlog.Open(logPath, logSize); err != nil {
		log.Fatal("failed to create log")
	}
	log.SetOutput(&rlog)

	// osu setup
	token, err := osuAuthorize()
	if err != nil {
		log.Fatal("acquiring code:", err)
	}

	osuClient, err := NewOsuClient(osuBotID, osuWatchChannel, token.Token, 3 * time.Minute)
	if err != nil {
		log.Fatal("osu client struct creation:", err)
	}

	// discord setup
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatal(err)
	}
	readDiscord := make(chan string, 32)
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID || m.ChannelID != discordWatchChannel {
			return
		}
		name := m.Author.GlobalName
		if len(name) == 0 {
			name = m.Author.Username
		}
		readDiscord <- fmt.Sprintf("%s: %s", name, m.Content)
	})
	dg.Identify.Intents = discordgo.IntentsGuildMessages
	err = dg.Open()
	if err != nil {
		log.Fatal(err)
	}

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)

	if err := osuClient.Open(); err != nil {
		log.Fatal("OsuClient.Open():", err)
	}

	for {
		select {
		case <-sigint:
			osuClient.Close()
			dg.Close()
			log.Println("done shutdown")
			os.Exit(0)
		case chat := <-osuClient.Read:
			if len(chat) > 0 && chat[0] == '-' {
				// stop discord from thinking it's a list item
				chat = "\\" + chat
			}
			dg.ChannelMessageSend(discordWatchChannel, chat)
		case chat := <-readDiscord:
			osuClient.Send(chat)
		}
	}
}
