package handlers

import (
	"github.com/memohai/memoh/internal/acpprofile"
	"github.com/memohai/memoh/internal/bots"
)

func scrubBotForResponse(bot bots.Bot) bots.Bot {
	bot.Metadata = acpprofile.ScrubMetadataForResponse(bot.Metadata)
	return bot
}

func scrubBotsForResponse(items []bots.Bot) []bots.Bot {
	out := make([]bots.Bot, 0, len(items))
	for _, item := range items {
		out = append(out, scrubBotForResponse(item))
	}
	return out
}
