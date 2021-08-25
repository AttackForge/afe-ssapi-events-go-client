package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	CONNECT   = iota
	RECONNECT = iota
	EXIT      = iota
)

var action chan int

type JsonRpcRequest struct {
	JsonRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      string      `json:"id"`
}

type JsonRpcResponse struct {
	JsonRPC string `json:"jsonrpc"`
	Result  string `json:"result"`
	ID      string `json:"id"`
}

type PendingRequest struct {
	request JsonRpcRequest
	success func(result interface{}, id string)
	failure func(error interface{}, id string)
	timeout *time.Timer
}

var connected bool = false
var heartbeatTimer *time.Timer
var pendingRequests map[string]PendingRequest

func notification(method string, params map[string]interface{}) {
	fmt.Println("method:", method)
	fmt.Println("params:")
	b, _ := json.MarshalIndent(params, "", "  ")
	fmt.Println(string(b))

	/* ENTER YOUR INTEGRATION CODE HERE */
	/* method contains the event type e.g. vulnerability-created */
	/* params contains the event body e.g. JSON object with timestamp & vulnerability details */
}

func connect() {
	if !connected {
		pendingRequests = make(map[string]PendingRequest)

		hostname, found := os.LookupEnv("HOSTNAME")

		if !found {
			fmt.Println("Environment variable HOSTNAME is undefined")
			action <- EXIT
			return
		}

		_, found = os.LookupEnv("EVENTS")

		if !found {
			fmt.Println("Environment variable EVENTS is undefined")
			action <- EXIT
			return
		}

		apiKey, found := os.LookupEnv("X_SSAPI_KEY")

		if !found {
			fmt.Println("Environment variable X_SSAPI_KEY is undefined")
			action <- EXIT
			return
		}

		port, found := os.LookupEnv("PORT")

		if !found {
			port = "443"
		}

		url := fmt.Sprintf("wss://%s:%s/api/ss/events", hostname, port)

		headers := make(http.Header)
		headers.Add("X-SSAPI-KEY", apiKey)

		dialer := websocket.DefaultDialer

		// uncomment the following to trust all certificates
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

		c, _, err := dialer.Dial(url, headers)

		if err != nil {
			fmt.Println(err)

			if c != nil {
				c.Close()
			}

			action <- RECONNECT
			return
		}

		connected = true

		heartbeat(c)
		subscribe(c)

		go listen(c)
	}
}

func listen(c *websocket.Conn) {
	defer c.Close()

	for {
		_, data, err := c.ReadMessage()

		if err != nil {
			fmt.Println(err)
			break
		}

		var payload map[string]interface{}

		err = json.Unmarshal(data, &payload)

		if err != nil {
			fmt.Println(err)
			continue
		}

		if payload["jsonrpc"] == "2.0" {
			method, method_ok := payload["method"]
			id, id_ok := payload["id"]
			result, result_ok := payload["result"]
			error, error_ok := payload["error"]

			if method_ok && !id_ok {
				params, params_ok := payload["params"]

				if params_ok {
					params, params_ok := params.(map[string]interface{})

					if params_ok {
						timestamp, timestamp_ok := params["timestamp"]

						if timestamp_ok {
							storeReplayTimestamp(timestamp.(string))
						}

					}
				}

				notification(method.(string), params.(map[string]interface{}))

			} else if method_ok && id_ok {
				if method == "heartbeat" {
					c.WriteJSON(JsonRpcResponse{
						JsonRPC: "2.0",
						Result:  time.Now().Format(time.RFC3339),
						ID:      id.(string),
					})

					heartbeat(c)
				}
			} else if result_ok && id_ok {
				pendingRequest, ok := pendingRequests[id.(string)]

				if ok {
					pendingRequest.timeout.Stop()
					pendingRequest.success(result, id.(string))
				}

			} else if error_ok && id_ok {
				pendingRequest, ok := pendingRequests[id.(string)]

				if ok {
					pendingRequest.timeout.Stop()
					pendingRequest.failure(error, id.(string))
				}
			}
		}
	}

	connected = false
	action <- RECONNECT
}

func heartbeat(c *websocket.Conn) {
	if heartbeatTimer != nil {
		heartbeatTimer.Stop()
	}

	heartbeatTimer = time.AfterFunc(31*time.Second, func() {
		c.Close()
		action <- RECONNECT
	})
}

func loadReplayTimestamp() string {
	timestamp := time.Now().Format(time.RFC3339)

	f, err := os.Open(".replay_timestamp")

	if err != nil {
		from, ok := os.LookupEnv("FROM")

		if ok {
			fmt.Println("Loaded replay timestamp from environment:", from)
			timestamp = from
		}
	} else {
		defer f.Close()

		data := make([]byte, 24)

		n, err := f.Read(data)

		if err != nil {
			fmt.Println(err)
		} else {
			if n == 24 {
				timestamp = string(data)
				fmt.Println("Loaded replay timestamp from storage:", timestamp)
			} else {
				fmt.Println("Invalid timestamp stored in \".replay_timestamp\"")
			}
		}
	}

	return timestamp
}

func storeReplayTimestamp(timestamp string) {
	f, err := os.Create(".replay_timestamp")

	if err != nil {
		fmt.Println(err)
		return
	}

	_, err = f.WriteString(timestamp)

	if err != nil {
		fmt.Println(err)
		return
	}
}

func subscribe(c *websocket.Conn) {
	events := strings.Split(os.Getenv("EVENTS"), ",")

	for i := range events {
		events[i] = strings.Trim(events[i], " ")
	}

	request := JsonRpcRequest{
		JsonRPC: "2.0",
		Method:  "subscribe",
		Params: struct {
			Events []string `json:"events"`
			From   string   `json:"from"`
		}{
			Events: events,
			From:   loadReplayTimestamp(),
		},
		ID: uuid.NewString(),
	}

	pendingRequests[request.ID] = PendingRequest{
		request: request,
		success: func(result interface{}, id string) {
			delete(pendingRequests, request.ID)
			fmt.Println("Subscribed to the following events:", result)
		},
		failure: func(error interface{}, id string) {
			delete(pendingRequests, request.ID)
			fmt.Printf("Subscription request %s failed - exiting\n", request.ID)

			action <- EXIT
		},
		timeout: time.AfterFunc(5*time.Second, func() {
			delete(pendingRequests, request.ID)
			fmt.Printf("Subscription request %s timed out - exiting\n", request.ID)

			action <- EXIT
		}),
	}

	c.WriteJSON(request)
}

func main() {
	pendingRequests = make(map[string]PendingRequest)

	action = make(chan int, 1)
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	action <- CONNECT

	for {
		select {
		case code := <-action:
			if code == CONNECT {
				connect()
			} else if code == RECONNECT {
				time.AfterFunc(time.Second, func() {
					action <- CONNECT
				})
			} else if code == EXIT {
				os.Exit(0)
			}

		case <-interrupt:
			action <- EXIT
		}
	}
}
