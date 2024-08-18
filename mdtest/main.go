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
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
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
    container     *sqlstore.Container
    dbLog         waLog.Logger
    eventChannel  chan interface{}
    ctx           context.Context
    cancel        context.CancelFunc
}

func NewClientManager(container *sqlstore.Container) *ClientManager {
    ctx, cancel := context.WithCancel(context.Background())
    return &ClientManager{
        dbLog:        waLog.Stdout("Database", logLevel, true),
        eventChannel: make(chan interface{}, 100),
        container:    container,
        ctx:          ctx,
        cancel:       cancel,
    }
}

// Update the getRegistrationID method
func (cm *ClientManager) getRegistrationID(sessionID string) (uint32, error) {
    return cm.container.GetSessionRegistrationID(sessionID)
}

func (cm *ClientManager) eventHandler(evt interface{}) {
    log.Infof("Event occurred: %+v", evt)
    select {
    case cm.eventChannel <- evt:
    default:
        log.Warnf("Event channel full, discarding event")
    }
}

func (cm *ClientManager) generateQRCode(ctx context.Context, sessionID string) (string, error) {
	//TODO remember JID and getDevice using it
    deviceStore, err := cm.container.GetFirstDevice()
    if err != nil {
        return "", fmt.Errorf("failed to get device: %v", err)
    }

    client := whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", logLevel, true))
    client.AddEventHandler(cm.eventHandler)

    qrChan, err := client.GetQRChannel(cm.ctx)
    if err != nil {
        if errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
            err = client.Connect()
            if err != nil {
                return "", fmt.Errorf("failed to connect to WhatsApp: %v", err)
            }
            return "", nil // No QR code needed, already connected
        }
        return "", fmt.Errorf("failed to get QR channel: %v", err)
    }

    select {
    case evt := <-qrChan:
		log.Infof("QR code event: %+v", evt)
        if evt.Event == "code" {
            png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256)
            if err != nil {
                return "", fmt.Errorf("failed to generate QR code: %v", err)
            }
            base64QR := base64.StdEncoding.EncodeToString(png)
            return base64QR, nil
        }
    case <-time.After(60 * time.Second):
        return "", fmt.Errorf("timeout waiting for QR code")
    }
    return "", fmt.Errorf("unexpected error generating QR code")
}

func (cm *ClientManager) checkAuthStatus(sessionID string) string {
	log.Infof("Checking auth status for session %s", sessionID)
    return "authenticated"
}


type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	SessionID string `json:"sessionID"`
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

	regID, err := cm.getRegistrationID(req.SessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

    deviceStore, err := cm.container.GetDeviceByRegistrationID(regID)
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

func (cm *ClientManager) getAllContactsHandler(w http.ResponseWriter, r *http.Request) {
    sessionID := r.URL.Query().Get("sessionID")
    if sessionID == "" {
        http.Error(w, "Session ID is required", http.StatusBadRequest)
        return
    }

    regID, err := cm.getRegistrationID(sessionID)
    if err != nil {
        http.Error(w, "Failed to get registration ID", http.StatusInternalServerError)
        return
    }

    deviceStore, err := cm.container.GetDeviceByRegistrationID(regID)
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

    store := client.Store.Contacts
    contacts, err := store.GetAllContacts()
    if err != nil {
        log.Errorf("Failed to get contacts: %v", err)
        http.Error(w, "Failed to get contacts", http.StatusInternalServerError)
        return
    }

    // Convert map[types.JID]types.ContactInfo to a slice for easier JSON serialization
    contactsList := make([]struct {
        JID  string           `json:"jid"`
        Info types.ContactInfo `json:"info"`
    }, 0, len(contacts))

	fmt.Printf("Number of contacts: %d\n", len(contacts))
	// for jid, info := range contacts {
	// 	contactsList = append(contactsList, struct {
	// 		JID  string           `json:"jid"`
	// 		Info types.ContactInfo `json:"info"`
	// 	}{
	// 		JID:  jid.String(),
	// 		Info: info,
	// 	})
	// 	fmt.Printf("Added contact: %s\n", jid.String())
	// }
	fmt.Printf("Number of items in contactsList: %d\n", len(contactsList))

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(contactsList)
}

func (cm *ClientManager) startClientSession(sessionID string) (<-chan string, <-chan interface{}, uint32, error) {
	qrCodeChan := make(chan string, 1)
    eventChan := make(chan interface{}, 100)

    deviceStore, err := cm.container.GetFirstDevice()
    if err != nil {
        return nil, nil, 0, fmt.Errorf("failed to get device: %v", err)
    }

    // Store the registration ID in the database
    err = cm.container.StoreSessionRegistrationID(sessionID, deviceStore.RegistrationID)
    if err != nil {
		log.Errorf("Failed to store registration ID: %v", err)
        return nil, nil, 0, fmt.Errorf("failed to store registration ID: %v", err)
    }

    client := whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", logLevel, true))
    client.AddEventHandler(func(evt interface{}) {
        eventChan <- evt
    })

    qrChan, err := client.GetQRChannel(cm.ctx)
    if err != nil {
        if errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
            err = client.Connect()
            if err != nil {
                return nil, nil, 0, fmt.Errorf("failed to connect to WhatsApp: %v", err)
            }
            return nil, nil, deviceStore.RegistrationID, nil 
        }
        return nil, nil, deviceStore.RegistrationID, fmt.Errorf("failed to get QR channel: %v", err)
    }

    go func() {
        for evt := range qrChan {
            if evt.Event == "code" {
                png, _ := qrcode.Encode(evt.Code, qrcode.Medium, 256)
                base64QR := base64.StdEncoding.EncodeToString(png)
                qrCodeChan <- base64QR
            }
        }
    }()

    go func() {
        err := client.Connect()
        if err != nil {
            log.Errorf("Failed to connect: %v", err)
            return
        }

        // Now wait for events indefinitely
        for {
            select {
            case <-cm.ctx.Done():
                return
            }
        }
    }()
    return qrCodeChan, eventChan, deviceStore.RegistrationID, nil
}

func (cm *ClientManager) handleQRCodeGeneration(w http.ResponseWriter, r *http.Request) {
    sessionID := uuid.New().String()
    qrCodeChan, _, registrationID, err := cm.startClientSession(sessionID)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    select {
    case qrCode := <-qrCodeChan:
        json.NewEncoder(w).Encode(map[string]string{
            "sessionID": sessionID,
            "qrCode": qrCode,
			"registrationID": fmt.Sprintf("%d", registrationID),
        })
    case <-time.After(30 * time.Second):
        http.Error(w, "Timeout waiting for QR code", http.StatusRequestTimeout)
    }
}

func (cm *ClientManager) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
    sessionID := r.URL.Query().Get("sessionID")
    
    // Set headers for SSE
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")

    // Check authentication status periodically
    for {
        select {
        case <-r.Context().Done():
            return
        case <-time.After(5 * time.Second):
            status := cm.checkAuthStatus(sessionID)
            fmt.Fprintf(w, "data: %s\n\n", status)
            w.(http.Flusher).Flush()
            
            if status == "authenticated" {
                return
            }
        }
    }
}

func main() {
	
	waBinary.IndentXML = true
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}

	log = waLog.Stdout("Main", logLevel, true)

	dbLog := waLog.Stdout("Database", logLevel, true)
    container, err := sqlstore.New("sqlite3", "file:../data/mdtest.db?_foreign_keys=on", dbLog)
    if err != nil {
        log.Errorf("Failed to connect to database: %v", err)
        return
    }
	manager := NewClientManager(container)

	router := gin.Default()
    router.Use(cors.New(cors.Config{
        AllowOrigins:     []string{"http://localhost:3000", "https://omnimap-seven.vercel.app"}, 
        AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
        AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type", "Authorization"},
        ExposeHeaders:    []string{"Content-Length"},
        AllowCredentials: true,
        MaxAge:           12 * time.Hour,
    }))

    router.POST("/send", gin.WrapF(manager.sendMessageHandler))
    // MAYBE POST REQUEST. 
	router.GET("/generate-qr", gin.WrapF(manager.handleQRCodeGeneration))
    router.GET("/auth-status", gin.WrapF(manager.handleAuthStatus))
	router.GET("/contacts", gin.WrapF(manager.getAllContactsHandler))

	router.Run(":8080")

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
