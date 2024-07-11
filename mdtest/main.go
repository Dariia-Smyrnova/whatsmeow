// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var log waLog.Logger

var logLevel = "INFO"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
// var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
// var dbAddress = flag.String("db-address", "file:mdtest.db?_foreign_keys=on", "Database address")
// var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")

type ClientManager struct {
    clients       map[string]*whatsmeow.Client
    mutex         sync.RWMutex
    dbLog         waLog.Logger
    eventChannel  chan interface{}
    container     *sqlstore.Container
    ctx           context.Context
    cancel        context.CancelFunc
}

func NewClientManager(container *sqlstore.Container) *ClientManager {
    ctx, cancel := context.WithCancel(context.Background())
    return &ClientManager{
        clients:      make(map[string]*whatsmeow.Client),
        dbLog:        waLog.Stdout("Database", logLevel, true),
        eventChannel: make(chan interface{}, 100),
        container:    container,
        ctx:          ctx,
        cancel:       cancel,
    }
}

func (cm *ClientManager) eventHandler(evt interface{}) {
    log.Infof("Event occurred: %+v", evt)
    select {
    case cm.eventChannel <- evt:
    default:
        log.Warnf("Event channel full, discarding event")
    }
}

type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	ClientID  string `json:"clientID"`
}

type SendMessageResponse struct {
    Success   bool   `json:"success"`
    Error     string `json:"error,omitempty"`
}

func (cm *ClientManager) sendMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SendMessageRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	recipient, ok := parseJID(req.Recipient)
	if !ok {
		json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Error: "Invalid recipient JID"})
		return
	}

	msg := &waProto.Message{
		Conversation: proto.String(req.Message),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
    defer cancel()

    deviceStore, err := cm.container.GetFirstDevice()
    if err != nil {
        log.Errorf("Failed to get device: %v", err)
        http.Error(w, "Internal server error", http.StatusInternalServerError)
        return
    }

    client := whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", logLevel, true))
    err = client.Connect()
    if err != nil {
        log.Errorf("Failed to connect: %v", err)
        http.Error(w, "Failed to connect", http.StatusInternalServerError)
        return
    }
    defer client.Disconnect()

    _, err = client.SendMessage(ctx, recipient, msg)
    if err != nil {
        log.Errorf("Failed to send message: %v", err)
        json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Error: err.Error()})
        return
    }

    json.NewEncoder(w).Encode(SendMessageResponse{Success: true})
}

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        // In production, implement proper origin checking
        return true
    },
    ReadBufferSize:  1024,
    WriteBufferSize: 1024,
}


func (cm *ClientManager) handleWebSocket(w http.ResponseWriter, r *http.Request) {
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Errorf("Failed to upgrade connection: %v", err)
        return
    }
    defer conn.Close()

    clientID := uuid.New().String()

    deviceStore, err := cm.container.GetFirstDevice()
    if err != nil {
        log.Errorf("Failed to get device: %v", err)
        conn.WriteJSON(map[string]string{"error": "Failed to get device"})
        return
    }

    client := whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", logLevel, true))
    client.AddEventHandler(cm.eventHandler)

    qrChan, err := client.GetQRChannel(cm.ctx)
    if err != nil {
        if errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
            err = client.Connect()
            if err != nil {
                log.Errorf("Failed to connect to WhatsApp: %v", err)
                conn.WriteJSON(map[string]string{"error": "Failed to connect to WhatsApp"})
                return
            }
            cm.mutex.Lock()
            cm.clients[clientID] = client
            cm.mutex.Unlock()
            return
        }
        conn.WriteJSON(map[string]string{"error": fmt.Sprintf("Failed to get QR channel: %v", err)})
        return
    }

    go func() {
        for evt := range qrChan {
            if evt.Event == "code" {
                png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256)
                if err != nil {
                    conn.WriteJSON(map[string]string{"error": fmt.Sprintf("Failed to generate QR code: %v", err)})
                    return
                }
                base64QR := base64.StdEncoding.EncodeToString(png)
                conn.WriteJSON(map[string]string{"qrCode": base64QR})
            } else {
                conn.WriteJSON(map[string]string{"status": evt.Event})
            }
        }
    }()

    go func() {
        for {
            select {
            case evt := <-cm.eventChannel:
                switch v := evt.(type) {
                case *events.Message:
                    conn.WriteJSON(map[string]interface{}{
                        "type": "message",
                        "data": v,
                    })
                case *events.Connected:
                    conn.WriteJSON(map[string]string{"status": "connected", "clientID": clientID})
                case *events.Disconnected:
                    conn.WriteJSON(map[string]string{"status": "disconnected"})
                }
            case <-cm.ctx.Done():
                return
            }
        }
    }()

    err = client.Connect()
    if err != nil {
        conn.WriteJSON(map[string]string{"error": fmt.Sprintf("Failed to connect: %v", err)})
        return
    }

    cm.mutex.Lock()
    cm.clients[clientID] = client
    cm.mutex.Unlock()

    // Implement ping/pong for keepalive
    go func() {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()

        for {
            select {
            case <-ticker.C:
                if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
                    log.Errorf("Failed to send ping: %v", err)
                    return
                }
            case <-cm.ctx.Done():
                return
            }
        }
    }()

    for {
        _, _, err := conn.ReadMessage()
        if err != nil {
            log.Errorf("WebSocket read error: %v", err)
            break
        }
    }

    cm.mutex.Lock()
    delete(cm.clients, clientID)
    cm.mutex.Unlock()
    client.Disconnect()
}

func main() {
	
	waBinary.IndentXML = true
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}

	log = waLog.Stdout("Main", logLevel, true)

	dbLog := waLog.Stdout("Database", logLevel, true)
    container, err := sqlstore.New("sqlite3", "file:mdtest.db?_foreign_keys=on", dbLog)
    if err != nil {
        log.Errorf("Failed to connect to database: %v", err)
        return
    }
	manager := NewClientManager(container)

	// Initialize Gin router
	router := gin.Default()
    router.Use(cors.New(cors.Config{
        AllowOrigins:     []string{"http://localhost:3000"}, // Update this for production
        AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
        AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type", "Authorization"},
        ExposeHeaders:    []string{"Content-Length"},
        AllowCredentials: true,
        MaxAge:           12 * time.Hour,
    }))

    router.POST("/send", gin.WrapF(manager.sendMessageHandler))
    router.GET("/ws", gin.WrapF(manager.handleWebSocket))

    srv := &http.Server{
        Addr:    ":8080",
        Handler: router,
    }

    go func() {
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Errorf("Failed to start server: %v", err)
        }
    }()

    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    log.Infof("Shutting down server...")
    manager.cancel()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if err := srv.Shutdown(ctx); err != nil {
        log.Errorf("Server forced to shutdown: %v", err)
    }

    log.Infof("Server exiting")
}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
			log.Errorf("Invalid JID %s: %v", arg, err)
			return recipient, false
		} else if recipient.User == "" {
			log.Errorf("Invalid JID %s: no server specified", arg)
			return recipient, false
		}
		return recipient, true
	}
}
