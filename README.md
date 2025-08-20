## usage

Before compilation, you will need:
* the go compiler
* this repository
* a Discord bot account with bot scope and send messages and view channel permissions
* an osu! account and oauth application
* to create the following files in the same directory as `main.go`
    * `oauth2secret`: for your osu! oauth application client secret
    * `oauth2id`: for your osu! oauth application ID
    * `discordtoken`: for your Discord bot token
* to make sure those files don't have trailing newlines; a lot of text editors add them; try `echo -n "something" > filename`

### compilation

On the first time, run `go mod tidy` to fetch dependencies.

To compile, run `go build`.

### execution

`./bridge`, then copy the link it prints and resolve it in your web browser of choice. Authorize your osu! account to be used. I'm just as disappointed as you for needing to do this overly complicated authentication, although if you had a true bot account you probably wouldn't need to.
