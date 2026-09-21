package main

import (
    "bytes"
    "encoding/json"
    "image"
    _ "image/jpeg"
    "image/png"
    "io"
    "log"
    "mime/multipart"
    "net/http"
    "net/http/httputil"
    "os"
    "strings"
    "time"

    "github.com/gotify/plugin-api"
    "github.com/gorilla/websocket"
)

// iconMaxSize is the max width/height (in px) the app icon is downscaled to
// before upload, so Telegram renders it as a small icon instead of a full-width photo.
const iconMaxSize = 128

// telegramCaptionLimit is Telegram's max length for a sendPhoto caption.
const telegramCaptionLimit = 1024

// telegramMessageLimit is how much text goes into a single sendMessage. Telegram
// counts 4096 UTF-16 units; counting runes instead leaves room for the surrogate
// pairs emoji take up.
const telegramMessageLimit = 4000

// markdownContentType is the Gotify content type whose messages are written as
// Markdown. Anything else is forwarded verbatim.
const markdownContentType = "text/markdown"

// GetGotifyPluginInfo returns gotify plugin info
func GetGotifyPluginInfo() plugin.Info {
    return plugin.Info{
    Version: "1.0",
    Author: "Anh Bui",
    Name: "Gotify 2 Telegram",
    Description: "Telegram message fowarder for gotify",
        ModulePath: "https://github.com/anhbh310/gotify2telegram",
    }
}

// Plugin is the plugin instance
type Plugin struct {
    ws *websocket.Conn;
    msgHandler plugin.MessageHandler;
    debugLogger *log.Logger;
    chatid string;
    telegram_bot_token string;
    gotify_host string;
    gotify_http_host string;
    gotify_client_token string;
    appImageCache map[uint32]string;
}

type GotifyMessage struct {
    Id uint32;
    Appid uint32;
    Message string;
    Title string;
    Priority uint32;
    Extras GotifyExtras `json:"extras"`;
}

// GotifyExtras carries the parts of a message's "extras" this plugin reads.
type GotifyExtras struct {
    ClientDisplay struct {
        ContentType string `json:"contentType"`
    } `json:"client::display"`
}

// parse_mode returns the Telegram parse mode for a message. Only a message
// Gotify itself marks as Markdown is parsed as Markdown; everything else is
// sent verbatim, so underscores in URLs and @usernames are not swallowed as
// italics (Telegram's legacy Markdown has no word-boundary rule, so a message
// containing t.me/some_chat and @some_user parses the whole span between them
// as italic and drops both underscores).
func (m *GotifyMessage) parse_mode() string {
    if strings.EqualFold(strings.TrimSpace(m.Extras.ClientDisplay.ContentType), markdownContentType) {
        return "Markdown"
    }
    return ""
}

// split_message cuts s into chunks of at most limit runes. Slicing by byte
// would cut multi-byte characters in half — a Cyrillic message sliced that way
// is invalid UTF-8, which Telegram rejects.
func split_message(s string, limit int) []string {
    if s == "" {
        return nil
    }
    runes := []rune(s)
    if limit <= 0 || len(runes) <= limit {
        return []string{s}
    }

    chunks := make([]string, 0, (len(runes)+limit-1)/limit)
    for i := 0; i < len(runes); i += limit {
        end := i + limit
        if end > len(runes) {
            end = len(runes)
        }
        chunks = append(chunks, string(runes[i:end]))
    }
    return chunks
}

type Application struct {
    Id    uint32 `json:"id"`
    Name  string `json:"name"`
    Image string `json:"image"`
}

type Payload struct {
    ChatID    string `json:"chat_id"`
    Text      string `json:"text"`
    ParseMode string `json:"parse_mode,omitempty"`
}

// redact strips secrets out of text before it's written to the log.
func (p *Plugin) redact(s string) string {
    out := s
    if p.telegram_bot_token != "" {
        out = strings.ReplaceAll(out, p.telegram_bot_token, "[REDACTED_BOT_TOKEN]")
    }
    if p.gotify_client_token != "" {
        out = strings.ReplaceAll(out, p.gotify_client_token, "[REDACTED_CLIENT_TOKEN]")
    }
    return out
}

func (p *Plugin) post_to_telegram(method string, data interface{}) (*http.Response, []byte, error) {
    payloadBytes, err := json.Marshal(data)
    if err != nil {
        return nil, nil, err
    }

    req, err := http.NewRequest("POST", "https://api.telegram.org/bot"+p.telegram_bot_token+"/"+method, bytes.NewBuffer(payloadBytes))
    if err != nil {
        return nil, nil, err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, nil, err
    }
    defer resp.Body.Close()

    bodyBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        return resp, nil, err
    }

    if resp.StatusCode != http.StatusOK {
        p.debugLogger.Println("============== Request ==============")
        pretty_print, dumpErr := httputil.DumpRequest(req, true)
        if dumpErr != nil {
            p.debugLogger.Printf("%v\n", dumpErr)
        }
        p.debugLogger.Printf("%s\n", p.redact(string(pretty_print)))

        p.debugLogger.Println("============== Response ==============")
        p.debugLogger.Printf("%s\n", string(bodyBytes))
    }

    return resp, bodyBytes, nil
}

func (p *Plugin) send_msg_to_telegram(msg string, parseMode string) {
    for _, sending_message := range split_message(msg, telegramMessageLimit) {
        data := Payload{
            ChatID:    p.chatid,
            Text:      sending_message,
            ParseMode: parseMode,
        }
        resp, _, err := p.post_to_telegram("sendMessage", data)
        if err != nil {
            p.debugLogger.Printf("Send request false: %v\n", err)
            return
        }

        if resp.StatusCode == http.StatusOK {
            p.debugLogger.Println("The message was forwarded successfully to Telegram")
            continue
        }

        if resp.StatusCode == http.StatusBadRequest && parseMode != "" {
            // The message may contain text that isn't valid Markdown; fall back to plain text
            // so the notification still arrives instead of being dropped.
            p.debugLogger.Println("Retrying as plain text after Markdown parse failure")
            data.ParseMode = ""
            resp, _, err = p.post_to_telegram("sendMessage", data)
            if err != nil {
                p.debugLogger.Printf("Send request false: %v\n", err)
                return
            }
            if resp.StatusCode == http.StatusOK {
                p.debugLogger.Println("The message was forwarded successfully to Telegram")
            }
        }
    }
}

// downscale_icon shrinks src so its longest side is at most iconMaxSize,
// so Telegram renders it as a small icon instead of a full-width photo.
// Images already at or below that size are returned unchanged.
func downscale_icon(src image.Image) image.Image {
    bounds := src.Bounds()
    w, h := bounds.Dx(), bounds.Dy()
    if w <= iconMaxSize && h <= iconMaxSize {
        return src
    }

    scale := float64(iconMaxSize) / float64(w)
    if hScale := float64(iconMaxSize) / float64(h); hScale < scale {
        scale = hScale
    }
    newW := int(float64(w) * scale)
    newH := int(float64(h) * scale)
    if newW < 1 {
        newW = 1
    }
    if newH < 1 {
        newH = 1
    }

    dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
    for y := 0; y < newH; y++ {
        for x := 0; x < newW; x++ {
            srcX := bounds.Min.X + x*w/newW
            srcY := bounds.Min.Y + y*h/newH
            dst.Set(x, y, src.At(srcX, srcY))
        }
    }
    return dst
}

// send_photo_to_telegram uploads the app icon with the notification text as its
// caption, so both arrive as a single Telegram message. Returns true once the
// photo itself was delivered (regardless of whether the caption needed a plain-text
// retry), so the caller knows not to send the caption text again separately.
func (p *Plugin) send_photo_to_telegram(photoURL string, caption string, parseMode string) bool {
    if photoURL == "" {
        p.debugLogger.Println("send_photo_to_telegram: no app image URL resolved, skipping")
        return false
    }
    p.debugLogger.Printf("send_photo_to_telegram: fetching app image from %s\n", photoURL)

    // Telegram's sendPhoto refuses to fetch URLs on localhost/private networks
    // (Gotify is typically only reachable there), so download the image
    // ourselves and upload the bytes instead of passing the URL along.
    imgResp, err := http.Get(photoURL)
    if err != nil {
        p.debugLogger.Printf("Fetch app image false: %v\n", err)
        return false
    }
    defer imgResp.Body.Close()
    if imgResp.StatusCode != http.StatusOK {
        p.debugLogger.Printf("Fetch app image false, status: %v\n", imgResp.Status)
        return false
    }
    p.debugLogger.Printf("send_photo_to_telegram: downloaded app image, content-type=%s content-length=%d\n", imgResp.Header.Get("Content-Type"), imgResp.ContentLength)

    iconBytes, err := shrink_icon(imgResp.Body)
    if err != nil {
        p.debugLogger.Printf("Resize app image false, sending original: %v\n", err)
        iconBytes, err = io.ReadAll(imgResp.Body)
        if err != nil {
            p.debugLogger.Printf("Read app image false: %v\n", err)
            return false
        }
    }

    for attempt := 0; attempt < 2; attempt++ {
        var buf bytes.Buffer
        writer := multipart.NewWriter(&buf)
        writer.WriteField("chat_id", p.chatid)
        writer.WriteField("caption", caption)
        if parseMode != "" {
            writer.WriteField("parse_mode", parseMode)
        }
        part, err := writer.CreateFormFile("photo", "app_icon.png")
        if err != nil {
            p.debugLogger.Printf("Create form file false: %v\n", err)
            return false
        }
        if _, err := part.Write(iconBytes); err != nil {
            p.debugLogger.Printf("Write app image false: %v\n", err)
            return false
        }
        if err := writer.Close(); err != nil {
            p.debugLogger.Printf("Close multipart writer false: %v\n", err)
            return false
        }

        req, err := http.NewRequest("POST", "https://api.telegram.org/bot"+p.telegram_bot_token+"/sendPhoto", &buf)
        if err != nil {
            p.debugLogger.Printf("Create photo request false: %v\n", err)
            return false
        }
        req.Header.Set("Content-Type", writer.FormDataContentType())
        p.debugLogger.Printf("send_photo_to_telegram: uploading %d bytes to Telegram sendPhoto (attempt %d)\n", len(iconBytes), attempt+1)

        resp, err := http.DefaultClient.Do(req)
        if err != nil {
            p.debugLogger.Printf("Send photo request false: %v\n", err)
            return false
        }
        bodyBytes, _ := io.ReadAll(resp.Body)
        resp.Body.Close()

        if resp.StatusCode == http.StatusOK {
            p.debugLogger.Println("The app image was forwarded successfully to Telegram")
            return true
        }

        p.debugLogger.Printf("Send photo false, status=%d response: %s\n", resp.StatusCode, p.redact(string(bodyBytes)))

        if resp.StatusCode == http.StatusBadRequest && parseMode != "" {
            // The caption may contain text that isn't valid Markdown; fall back to
            // plain text so the photo still arrives instead of being dropped.
            p.debugLogger.Println("Retrying photo caption as plain text after Markdown parse failure")
            parseMode = ""
            continue
        }
        return false
    }
    return false
}

// shrink_icon decodes an image and re-encodes it as PNG at icon size.
func shrink_icon(r io.Reader) ([]byte, error) {
    img, _, err := image.Decode(r)
    if err != nil {
        return nil, err
    }

    var buf bytes.Buffer
    if err := png.Encode(&buf, downscale_icon(img)); err != nil {
        return nil, err
    }
    return buf.Bytes(), nil
}

func (p *Plugin) get_app_image_url(appid uint32) string {
    if imageURL, ok := p.appImageCache[appid]; ok {
        p.debugLogger.Printf("get_app_image_url: cache hit for appid=%d\n", appid)
        return imageURL
    }

    p.debugLogger.Printf("get_app_image_url: fetching %s/application\n", p.gotify_http_host)
    req, err := http.NewRequest("GET", p.gotify_http_host+"/application", nil)
    if err != nil {
        p.debugLogger.Printf("Create application request false: %v\n", err)
        return ""
    }
    req.Header.Set("X-Gotify-Key", p.gotify_client_token)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        p.debugLogger.Printf("Fetch applications false: %v\n", err)
        return ""
    }
    defer resp.Body.Close()
    p.debugLogger.Printf("get_app_image_url: /application responded with status=%d\n", resp.StatusCode)

    var apps []Application
    if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
        p.debugLogger.Printf("Decode applications false: %v\n", err)
        return ""
    }
    p.debugLogger.Printf("get_app_image_url: decoded %d application(s)\n", len(apps))

    for _, app := range apps {
        p.appImageCache[app.Id] = p.gotify_http_host + "/" + app.Image
        p.debugLogger.Printf("get_app_image_url: appid=%d name=%q image=%s\n", app.Id, app.Name, p.appImageCache[app.Id])
    }

    imageURL, found := p.appImageCache[appid]
    if !found {
        p.debugLogger.Printf("get_app_image_url: appid=%d not found in application list\n", appid)
    }
    return imageURL
}

// split_caption splits text into the part that rides along as the photo caption
// and whatever is left over, counting runes so a multi-byte character is never
// cut in half.
func split_caption(text string) (string, string) {
    runes := []rune(text)
    if len(runes) <= telegramCaptionLimit {
        return text, ""
    }
    return string(runes[:telegramCaptionLimit]), string(runes[telegramCaptionLimit:])
}

// forward_to_telegram sends the app icon and text as a single Telegram message
// (icon with text as its caption) when the text fits Telegram's caption limit.
// Longer text is split: the first chunk rides as the caption, the rest follows
// as normal chunked messages. If there's no icon, or sending the photo fails,
// it falls back to plain chunked text messages.
func (p *Plugin) forward_to_telegram(appid uint32, text string, parseMode string) {
    photoURL := p.get_app_image_url(appid)
    if photoURL == "" {
        p.send_msg_to_telegram(text, parseMode)
        return
    }

    caption, remaining := split_caption(text)

    if !p.send_photo_to_telegram(photoURL, caption, parseMode) {
        p.send_msg_to_telegram(text, parseMode)
        return
    }

    if remaining != "" {
        p.send_msg_to_telegram(remaining, parseMode)
    }
}

func (p *Plugin) connect_websocket() {
    for {
        ws, _, err := websocket.DefaultDialer.Dial(p.gotify_host, nil)
        if err == nil {
            p.ws = ws
            break
        }
        p.debugLogger.Printf("Cannot connect to websocket: %v\n", p.redact(err.Error()))
        time.Sleep(5 * time.Second)
    }
    p.debugLogger.Println("WebSocket connected successfully, ready for forwarding")
}

func (p *Plugin) get_websocket_msg(url string, token string) {
    p.gotify_host = url + "/stream?token=" + token
    p.gotify_http_host = strings.Replace(strings.Replace(url, "wss://", "https://", 1), "ws://", "http://", 1)
    p.gotify_client_token = token
    p.appImageCache = make(map[uint32]string)
    p.chatid = os.Getenv("TELEGRAM_CHAT_ID")
    p.telegram_bot_token = os.Getenv("TELEGRAM_BOT_TOKEN")
    p.debugLogger.Printf("chatid set: %v, bot token set: %v, gotify client token set: %v\n", p.chatid != "", p.telegram_bot_token != "", p.gotify_client_token != "")
    p.debugLogger.Printf("gotify_host: %s, gotify_http_host: %s\n", p.redact(p.gotify_host), p.gotify_http_host)

    go p.connect_websocket()

    for {
        msg := &GotifyMessage{}
        if p.ws == nil {
            time.Sleep(3 * time.Second)
            continue
        }
        err := p.ws.ReadJSON(msg)
        if err != nil {
            p.debugLogger.Printf("Error while reading websocket: %v\n", err)
            p.connect_websocket()
            continue
        }
        p.forward_to_telegram(msg.Appid, msg.Title+"\n\n"+msg.Message, msg.parse_mode())
    }
}

// SetMessageHandler implements plugin.Messenger
// Invoked during initialization
func (p *Plugin) SetMessageHandler(h plugin.MessageHandler) {
    p.debugLogger = log.New(os.Stdout, "Gotify 2 Telegram: ", log.Lshortfile)
    p.msgHandler = h
}

func (p *Plugin) Enable() error {
    go p.get_websocket_msg(os.Getenv("GOTIFY_HOST"), os.Getenv("GOTIFY_CLIENT_TOKEN"))
    return nil
}

// Disable implements plugin.Plugin
func (p *Plugin) Disable() error {
    if p.ws != nil {
        p.ws.Close()
    }
    return nil
}

// NewGotifyPluginInstance creates a plugin instance for a user context.
func NewGotifyPluginInstance(ctx plugin.UserContext) plugin.Plugin {
    return &Plugin{}
}

func main() {
    panic("this should be built as go plugin")
}
