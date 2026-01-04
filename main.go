/*
Package main provides an experimental Fox Hunt RAG and LLM server.

Use with fox:

	fox hunt -uhttp://0.0.0.0:8211/event *.evtx

Query server:

	curl -X POST 0.0.0.0:8211/query -d "are there critical events?"
*/
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/philippgille/chromem-go"
	"github.com/zeebo/xxh3"
)

const Prompt = `
You are a helpful digital forensic analyst and expert witness, tasked with answering questions about text based log lines. Answer the given question solely based on the provided context. Answer the question in a very concise manner. Use an unbiased and professional tone. Cite relevant lines starting with their timestamp.

The lines are in Common Event Format (CEF) and not part of the conversation with the user. The lines are not in chronological order and start with a timestamp followed by the hostname and the message.

If you can't the answer the question based on the provided context, answer with: "This information is not available". Do not repeat text. Don't make anything up.

If sure about something, answer with "It is CERTAIN ...".

If unsure about something, answer with "It APPEARS ...".
`
const Query = `
This is the question:
%s

This is the context:
%s
`
const Model = "mistral"
const Embed = "nomic-embed-text"

var db = chromem.NewDB()
var stream = false
var messages []api.Message
var keepAlive = &api.Duration{Duration: time.Hour}

func history(role, msg string) {
	messages = append(messages, api.Message{
		Role:    role,
		Content: msg,
	})
}

func consume(events chan string) {
	fn := chromem.NewEmbeddingFuncOllama(Embed, "")

	col, err := db.GetOrCreateCollection("fox", nil, fn)

	if err != nil {
		panic(err)
	}

	for event := range events {
		err = col.AddDocument(context.Background(), chromem.Document{
			ID:      fmt.Sprintf("%x", xxh3.HashString(event)),
			Content: event,
		})

		if err != nil {
			panic(err)
		}
	}
}

func preload(client *api.Client) {
	err := client.Chat(context.Background(), &api.ChatRequest{
		Model:     Model,
		KeepAlive: keepAlive,
	}, func(_ api.ChatResponse) error {
		return nil // preloaded model
	})

	if err != nil {
		panic(err)
	}
}

func query(client *api.Client, input string) chan string {
	col := db.GetCollection("fox", nil)

	res, err := col.Query(context.Background(), input, col.Count(), nil, nil)

	if err != nil {
		panic(err)
	}

	var events string

	for _, r := range res {
		events += r.Content + "\n"
	}

	history("User", fmt.Sprintf(Query, input, events))

	req := &api.ChatRequest{
		Model:     Model,
		Stream:    &stream,
		Messages:  messages,
		KeepAlive: keepAlive,
		Options: map[string]any{
			"num_ctx":     4096,
			"temperature": 0.2,
			"seed":        8211,
			"top_k":       10,
			"top_p":       0.5,
		},
	}

	answer := make(chan string, 1)

	go func() {
		err = client.Chat(context.Background(), req, func(res api.ChatResponse) error {
			history("Assistant", res.Message.Content)

			answer <- res.Message.Content

			close(answer)
			return nil
		})

		if err != nil {
			panic(err)
		}
	}()

	return answer
}

func main() {
	var events = make(chan string, 4096)

	client, err := api.ClientFromEnvironment()

	if err != nil {
		panic(err)
	}

	go preload(client)

	go consume(events)

	history("System", Prompt)

	server := gin.Default()

	server.GET("/event", func(c *gin.Context) {
		col := db.GetCollection("fox", nil)

		count := fmt.Sprintf("%d events", col.Count())

		c.String(http.StatusOK, count)
	})

	server.POST("/event", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)

		if err != nil {
			_ = c.Error(err)
			return
		}

		events <- string(body)

		c.Status(http.StatusOK)
	})

	server.POST("/query", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)

		if err != nil {
			_ = c.Error(err)
			return
		}

		answer := <-query(client, string(body))

		c.String(http.StatusOK, answer)
	})

	err = server.Run("0.0.0.0:8211")

	if err != nil {
		panic(err)
	}

	close(events)
}
