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
	osuWatchChannel = 58277602
	discordWatchChannel = "1407449989969477742"

	logPath = "./log"
	logSize = 1048576
)

//go:embed oauth2id
var oauth2ID string
//go:embed oauth2secret
var oauth2Secret string
//go:embed discordtoken
var discordToken string
//go:embed webhookid
var webhookID string
//go:embed webhooktoken
var webhookToken string

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

	osuClient, err := NewOsuClient(osuBotID, osuWatchChannel, token.Token, 15 * time.Second)
	if err != nil {
		log.Fatal("osu client struct creation:", err)
	}

	// discord setup
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatal("discordgo.New():", err)
	}
	readDiscord := make(chan string, 32)
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID || m.ChannelID != discordWatchChannel || m.WebhookID == webhookID {
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
		log.Fatal("discordgo:Session.Open():", err)
	}

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)

	if err := osuClient.Open(); err != nil {
		log.Fatal("OsuClient.Open():", err)
	}

	shutdown := func() {
		osuClient.Close()
		dg.Close()
		log.Println("done shutdown")
		os.Exit(0)
	}
	pingPerms := discordgo.MessageAllowedMentions{
		Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers},
	}
	for {
		select {
		case <-sigint:
			shutdown()
		case chat, ok := <-osuClient.Read:
			if !ok {
				shutdown()
			}
			_, err := dg.WebhookExecute(webhookID, webhookToken, false, &discordgo.WebhookParams{
				Content: chat.Content,
				Username: chat.Author,
				AvatarURL: chat.AvatarURL,
				AllowedMentions: &pingPerms,
			})
			if err != nil {
				log.Println("discordgo:Session.WebhookExecute():", err)
				if len(chat.Author) > 0 && chat.Author[0] == '-' {
					chat.Author = "\\" + chat.Author
				}
				dg.ChannelMessageSend(discordWatchChannel, chat.Author + ": " + chat.Content)
			}
		case chat, ok := <-readDiscord:
			if !ok {
				shutdown()
			}
			osuClient.Send(chat)
		}
	}
}
