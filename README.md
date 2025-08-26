## usage

Before compilation, you will need:
* the go compiler
* this repository
* a Discord bot account with bot scope and send messages and view channel permissions
* an osu! account and oauth application
    * http://localhost:7538 should be one of the callback URLs
* to create the following files in the same directory as `main.go`
    * `oauth2secret`: for your osu! oauth application client secret
    * `oauth2id`: for your osu! oauth application ID
    * `discordtoken`: for your Discord bot token
    * `webhookID`: for your Discord webhook ID
    * `webhookToken`: for your Discord webhook token
* to make sure those files don't have trailing newlines; a lot of text editors add them; try `echo -n "something" > filename`
* to set the following constants in `main.go`:
    * `osuBotID` (int): to be the account ID of the bot
    * `osuWatchChannel` (int): to be the ID of the osu! channel to relay
    * `discordWatchChannel` (string): to be the ID of the Discord channel to relay
* to set the following constants in `osuauthhelper.go`, if the machine you authorize the osu! account from is not the machine you run the program on (the server):
    * `codeGetterEscaped` (string): the escaped URL of the server
    * `codeGetterAddr` (string): the port of the server
    * `useTLS` (bool): set to true
    * additionally, place `key.pem` and `cert.pem` (self signed or not) in the same directory as the executable after you build it
* to optionally change the size of the circular log (default 1 MiB), set `logSize` (int) in `main.go` to another value in bytes

### compilation

To compile, run `go build`; this will automatically fetch dependencies you don't have.

### execution

`./bridge`, then copy the link it prints and resolve it in your web browser of choice. Authorize your osu! account to be used. I'm just as disappointed as you for needing to do this overly complicated authentication, although if you had a true bot account you probably wouldn't need to.
