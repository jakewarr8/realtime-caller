package main

import (
	"os"
	"fmt"
	"log"
	"net/http"
	"sync"
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/twilio/twilio-go"
	"github.com/joho/godotenv"
	api "github.com/twilio/twilio-go/rest/api/v2010"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins
	},
}

type ConnectionPair struct {
	conn1 *websocket.Conn
	conn2 *websocket.Conn
	mu    sync.Mutex
	ready bool
	ssid string
}

var connectionPair = &ConnectionPair{}

func main () {
	fmt.Println("realtime-caller")

	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	setupRoutes()

	numberto := os.Getenv("PHONE_NUMBER_TO")
	go makeCall(numberto)

	log.Fatal(http.ListenAndServe(":8080", nil))

}

func setupRoutes() {
    http.HandleFunc("/", homePage)
    http.HandleFunc("/ws", wsEndpoint)
}

func homePage(w http.ResponseWriter, r *http.Request) {
	fmt.Println("hp enpoint hit")
	fmt.Fprintf(w, "Home Page")
}

func wsEndpoint(w http.ResponseWriter, r *http.Request) {
	fmt.Println("ws enpoint hit")
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Error upgrading first connection:", err)
		return
	}
	fmt.Println("First connection established.")

	connectionPair.mu.Lock()
	connectionPair.conn1 = conn
	connectionPair.ready = true
	connectionPair.mu.Unlock()
	
	// establish ws client with openai
	dialWs()
}

func dialWs() {
	connectionPair.mu.Lock()
	defer connectionPair.mu.Unlock()
	if !connectionPair.ready || connectionPair.conn1 == nil {
		fmt.Println("First connection not established yet")
		return
	}

  	secretKey := os.Getenv("OPENAI_API_KEY")
	serverAddr := "wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview-2024-12-17"
	headers := http.Header{}
	headers.Add("Authorization", "Bearer " + secretKey)
	headers.Add("OpenAI-Beta", "realtime=v1")
	

	// Connect to the WebSocket server
	conn, _, err := websocket.DefaultDialer.Dial(serverAddr, headers)
	if err != nil {
		fmt.Println("Error dialing connection:", err)
		return
	}
	fmt.Println("WS dial connection established.")
	
	connectionPair.conn2 = conn
	
	// initalize session with openai websocket using json config

	session_update := `{
        "type": "session.update",
        "session": {
            "turn_detection": {"type": "server_vad"},
            "input_audio_format": "g711_ulaw",
            "output_audio_format": "g711_ulaw",
            "voice": "echo",
            "instructions": "You are making a phone call to place an order for a pizza. You love pineapple but not onions.",
            "modalities": ["text", "audio"],
            "temperature": 0.8
        }
    }`

	err = conn.WriteMessage(websocket.TextMessage, []byte(session_update))
	if err != nil {
		fmt.Println("Error writing config to connection:", err)
		return
	}

	pipeConnections(connectionPair.conn1, connectionPair.conn2)
}

type BaseEvent struct {
	Event string `json:"event"`
	StreamSid string `json:"streamSid"`
}

type EventMedia struct {
	Event          string `json:"event"`
	SequenceNumber string `json:"sequenceNumber"`
	Media          struct {
		Track     string `json:"track"`
		Chunk     string `json:"chunk"`
		Timestamp string `json:"timestamp"`
		Payload   string `json:"payload"`
	} `json:"media"`
	StreamSid string `json:"streamSid"`
}

type AudioAppend struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

type AiEvent struct {
	Type         string `json:"type"`
}

type AiDeltaEvent struct {
	Type         string `json:"type"`
	Delta        string `json:"delta"`
}

type TwilioPayload struct {
	Event     string `json:"event"`
	StreamSid string `json:"streamSid"`
	Media     MediaPayload `json:"media"`
}

type MediaPayload struct {
	Payload string `json:"payload"`
}

func pipeConnections(conn1, conn2 *websocket.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	// read data from twilio write to openai
	go func() {
		defer wg.Done()
		defer conn1.Close()
		defer conn2.Close()

		for {
			messageType, message, err := conn1.ReadMessage()
			if err != nil {
				fmt.Println("Error reading from conn1:", err)
				break
			}
			

			var baseEvent BaseEvent
			if err := json.Unmarshal(message, &baseEvent); err != nil {
				fmt.Println("Invalid JSON")
				return
			}
			
			//read twilio event type
			if baseEvent.Event == "media" {
				var eventMedia EventMedia
				if err := json.Unmarshal(message, &eventMedia); err != nil {
					fmt.Println("Invalid JSON")
					return
				}

				audio, err := json.Marshal(AudioAppend{Type: "input_audio_buffer.append", Audio: eventMedia.Media.Payload})
				if err != nil {
					fmt.Println("Invalid Marshal JSON")
				}
				err = conn2.WriteMessage(messageType, audio)
				if err != nil {
					fmt.Println("Error writing to conn2:", err)
					break
				}

			} else if baseEvent.Event == "start" {
				connectionPair.ssid = baseEvent.StreamSid
				fmt.Println("Incoming stream started ", connectionPair.ssid)
			} else {
				fmt.Println("msg from twilio")
				fmt.Println(string(message))
			}		
		}
	}()
	
	// read data from openai write to twilio
	go func() {
		defer wg.Done()
		defer conn1.Close()
		defer conn2.Close()

		for {
			messageType, message, err := conn2.ReadMessage()
			if err != nil {
				fmt.Println("Error reading from conn2:", err)
				break
			}
			
			var response AiEvent
			if err := json.Unmarshal(message, &response); err != nil {
				fmt.Println("Invalid JSON")
				return
			}

			if response.Type == "response.audio.delta" {
				var eventDelta AiDeltaEvent
				if err := json.Unmarshal(message, &eventDelta); err != nil {
					fmt.Println("Invalid JSON")
					return
				}

				twilioPayload, err := json.Marshal(TwilioPayload{
					Event: "media", 
					StreamSid: connectionPair.ssid, 
					Media: MediaPayload{
						Payload: eventDelta.Delta,
					}})
				if err != nil {
					fmt.Println("Invalid Marshal JSON")
				}

				//fmt.Println("payload from openai", twilioPayload)
				
				err = conn1.WriteMessage(messageType, twilioPayload)
				if err != nil {
					fmt.Println("Error writing to conn1:", err)
					break
				}
			} else {
				fmt.Println("msg from openai")
				fmt.Println(string(message))
			}
		}
	}()

	wg.Wait()
	fmt.Println("Connections closed.")
}

func makeCall(numto string) error {
	// Find your Account SID and Auth Token at twilio.com/console
	// and set the environment variables. See http://twil.io/secure
	// Make sure TWILIO_ACCOUNT_SID and TWILIO_AUTH_TOKEN exists in your environment
	client := twilio.NewRestClient()

	domain := os.Getenv("DOMAIN")
	numberfrom := os.Getenv("PHONE_NUMBER_FROM")

	outbound_twiml := `<?xml version="1.0" encoding="UTF-8"?><Response><Connect><Stream url="wss://` + domain + `/ws" /></Connect></Response>`

	params := &api.CreateCallParams{}
	params.SetTo(numto)
	params.SetFrom(numberfrom)
	params.SetTwiml(outbound_twiml)

	resp, err := client.Api.CreateCall(params)

	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	} else {
		if resp.Sid != nil {
			fmt.Println("made call 1",*resp.Sid)
		} else {
			fmt.Println("made call 2", resp.Sid)
		}
	}

	return nil
}