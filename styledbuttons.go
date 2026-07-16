package main

import (
	"encoding/json"
	"strings"

	tele "gopkg.in/telebot.v4"
)

type sBtn struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
	Style        string `json:"style,omitempty"`
}

func sData(text, unique, style string, data ...string) sBtn {
	cb := "\f" + unique
	if len(data) > 0 && data[0] != "" {
		cb += "|" + strings.Join(data, "|")
	}
	return sBtn{Text: text, CallbackData: cb, Style: style}
}

func sURL(text, url, style string) sBtn {
	return sBtn{Text: text, URL: url, Style: style}
}

func sRow(btns ...sBtn) []sBtn { return btns }

func styledMarkupJSON(rows ...[]sBtn) string {
	type kb struct {
		InlineKeyboard [][]sBtn `json:"inline_keyboard"`
	}
	b, _ := json.Marshal(kb{InlineKeyboard: rows})
	return string(b)
}

func sendStyled(bot *tele.Bot, chatID int64, text, markupJSON, parseMode string) {
	params := map[string]string{
		"chat_id":      jsonInt(chatID),
		"text":         text,
		"reply_markup": markupJSON,
	}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	bot.Raw("sendMessage", params)
}

func sendStyledMsg(bot *tele.Bot, chat *tele.Chat, text, markupJSON, parseMode string) {
	sendStyled(bot, chat.ID, text, markupJSON, parseMode)
}

func sendStyledAnimationURL(bot *tele.Bot, chatID int64, animURL, caption, markupJSON, parseMode string) {
	params := map[string]string{
		"chat_id":      jsonInt(chatID),
		"animation":    animURL,
		"caption":      caption,
		"reply_markup": markupJSON,
	}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	bot.Raw("sendAnimation", params)
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
