package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
	"github.com/getlantern/systray"
)

const (
	serverAddr   = "0.0.0.0:50059"
	kokoroAPI    = "http://localhost:8880/v1/audio/speech"
	maxChunkSize = 1000
)

// QueueRequest represents the incoming JSON payload
type QueueRequest struct {
	Text string `json:"text"`
}

// KokoroRequest represents the TTS API request
type KokoroRequest struct {
	Model string `json:"model"`
	Voice string `json:"voice"`
	Input string `json:"input"`
}

// QueueItem represents an item in the playback queue
type QueueItem struct {
	Type      string // "text" or "audio"
	Text      string
	AudioData []byte
}

// TTSApp is the main application structure
type TTSApp struct {
	queue         []QueueItem
	queueMu       sync.Mutex
	server        *http.Server
	ctx           context.Context
	cancel        context.CancelFunc
	currentText   string
	currentTextMu sync.RWMutex
	isPaused      bool
	pauseMu       sync.Mutex
	pauseCond     *sync.Cond
	stopCurrent   bool
	stopCurrentMu sync.Mutex
	volume        float64
	volumeMu      sync.RWMutex
	replayRequest bool
	replayMu      sync.Mutex

	speakerInit   bool
	speakerInitMu sync.Mutex

	lastConcatenatedAudio []byte
	lastAudioMu           sync.Mutex
}

func main() {
	app := &TTSApp{
		queue:  make([]QueueItem, 0),
		volume: 0.5, // Default medium volume
	}
	app.ctx, app.cancel = context.WithCancel(context.Background())
	app.pauseCond = sync.NewCond(&app.pauseMu)

	// Start the worker goroutine
	go app.worker()

	// Start the HTTP server
	go app.startServer()

	// Run systray (this blocks until Quit is called)
	systray.Run(app.onReady, app.onExit)

}

// startServer initializes and starts the HTTP server
func (app *TTSApp) startServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", app.handleQueue)
	mux.HandleFunc("/play", app.handlePlay)

	app.server = &http.Server{
		Addr:    serverAddr,
		Handler: mux,
	}

	log.Printf("Starting HTTP server on %s", serverAddr)
	if err := app.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// handleQueue processes POST requests to the /queue endpoint
func (app *TTSApp) handleQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req QueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Text == "" {
		http.Error(w, "Text field is required", http.StatusBadRequest)
		return
	}

	// Add text to queue
	app.queueMu.Lock()
	app.queue = append(app.queue, QueueItem{
		Type: "text",
		Text: req.Text,
	})
	app.queueMu.Unlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	log.Printf("Text queued: %s", req.Text)
}

// handlePlay processes POST requests to the /play endpoint
func (app *TTSApp) handlePlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit body size to avoid memory issues (e.g., 50MB)
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	audioData, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	if len(audioData) == 0 {
		http.Error(w, "Empty body", http.StatusBadRequest)
		return
	}

	// Add audio to queue
	app.queueMu.Lock()
	app.queue = append(app.queue, QueueItem{
		Type:      "audio",
		AudioData: audioData,
	})
	app.queueMu.Unlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "queued", "type": "audio"})
	log.Printf("Audio queued, size: %d bytes", len(audioData))
}

// worker continuously processes items from the queue
func (app *TTSApp) worker() {
	for {
		select {
		case <-app.ctx.Done():
			return
		default:
			// Check for replay request
			app.replayMu.Lock()
			if app.replayRequest {
				app.replayRequest = false
				app.replayMu.Unlock()

				app.lastAudioMu.Lock()
				audioData := app.lastConcatenatedAudio
				app.lastAudioMu.Unlock()

				if len(audioData) > 0 {
					// Reset stop flag
					app.stopCurrentMu.Lock()
					app.stopCurrent = false
					app.stopCurrentMu.Unlock()
					if err := app.playAudio(audioData); err != nil {
						log.Printf("Error replaying audio: %v", err)
					}
				}
				continue // back to start of loop
			}
			app.replayMu.Unlock()

			app.queueMu.Lock()
			if len(app.queue) == 0 {
				app.queueMu.Unlock()
				time.Sleep(100 * time.Millisecond)
				continue
			}
			item := app.queue[0]
			app.queue = app.queue[1:]
			app.queueMu.Unlock()

			switch item.Type {
			case "text":
				app.processText(item.Text)
			case "audio":
				// Try to play as is (MP3)
				if err := app.playAudio(item.AudioData); err != nil {
					log.Printf("Direct playback failed, trying MP4 extraction: %v", err)
					// Try to extract from MP4
					if audioData, err := extractMP4Audio(item.AudioData); err == nil {
						if err := app.playAudio(audioData); err != nil {
							log.Printf("Error playing extracted audio: %v", err)
						} else {
							log.Println("Successfully played extracted audio from MP4")
						}
					} else {
						log.Printf("Failed to extract audio from MP4: %v", err)
					}
				}
			}
		}
	}
}

// processText splits text into chunks, gets audio for each, concatenates, and plays
func (app *TTSApp) processText(text string) {
	chunks := splitTextSmartly(text, maxChunkSize)
	var allAudioData [][]byte

	for _, chunk := range chunks {
		// Check if we should stop
		app.stopCurrentMu.Lock()
		if app.stopCurrent {
			app.stopCurrent = false
			app.stopCurrentMu.Unlock()
			return
		}
		app.stopCurrentMu.Unlock()

		app.currentTextMu.Lock()
		app.currentText = chunk
		app.currentTextMu.Unlock()

		audioData, err := app.getChunkAudio(chunk)
		if err != nil {
			log.Printf("Error getting audio for chunk: %v", err)
			continue
		}
		allAudioData = append(allAudioData, audioData)
	}

	if len(allAudioData) > 0 {
		concatenatedAudio := bytes.Join(allAudioData, []byte{})
		app.lastAudioMu.Lock()
		app.lastConcatenatedAudio = concatenatedAudio
		app.lastAudioMu.Unlock()
		if err := app.playAudio(concatenatedAudio); err != nil {
			log.Printf("Error playing concatenated audio: %v", err)
		}
	}

	app.currentTextMu.Lock()
	app.currentText = ""
	app.currentTextMu.Unlock()
}

// getChunkAudio sends a text chunk to Kokoro TTS API and returns the audio data
func (app *TTSApp) getChunkAudio(text string) ([]byte, error) {
	log.Printf("Generating audio for text chunk: %s", text[:min(50, len(text))])

	// Create API request
	reqBody := KokoroRequest{
		Model: "kokoro",
		Voice: "af_sky+af_bella",
		Input: text,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Make HTTP request
	resp, err := http.Post(kokoroAPI, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to call TTS API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TTS API returned status %d", resp.StatusCode)
	}

	// Read audio data
	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read audio data: %w", err)
	}

	return audioData, nil
}

// playAudio plays MP3 audio data from memory with pause/stop support
func (app *TTSApp) playAudio(audioData []byte) error {
	// Decode MP3
	reader := bytes.NewReader(audioData)
	streamer, format, err := mp3.Decode(io.NopCloser(reader))
	if err != nil {
		return fmt.Errorf("failed to decode MP3: %w", err)
	}
	defer streamer.Close()

	// Initialize speaker if not already done
	app.speakerInitMu.Lock()
	if !app.speakerInit {
		speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
		app.speakerInit = true
	}
	app.speakerInitMu.Unlock()

	// Resample if necessary
	resampled := beep.Resample(4, format.SampleRate, format.SampleRate, streamer)

	// Create control wrapper for pause
	ctrl := &beep.Ctrl{Streamer: resampled, Paused: false}

	// Create custom volume control streamer
	volumeStreamer := &VolumeStreamer{
		Streamer: ctrl,
		app:      app,
	}

	// Monitor for pause/stop changes
	done := make(chan bool)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// Check for stop
				app.stopCurrentMu.Lock()
				stopped := app.stopCurrent
				app.stopCurrentMu.Unlock()
				if stopped {
					speaker.Clear()
					done <- true
					return
				}

				// Check for pause
				app.pauseMu.Lock()
				paused := app.isPaused
				app.pauseMu.Unlock()

				speaker.Lock()
				ctrl.Paused = paused
				speaker.Unlock()
			}
		}
	}()

	// Play the audio
	speaker.Play(beep.Seq(volumeStreamer, beep.Callback(func() {
		done <- true
	})))

	<-done
	return nil
}

// VolumeStreamer applies volume control to an audio stream
type VolumeStreamer struct {
	Streamer beep.Streamer
	app      *TTSApp
}

func (v *VolumeStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	n, ok = v.Streamer.Stream(samples)
	if !ok {
		return n, ok
	}

	// Apply volume
	v.app.volumeMu.RLock()
	vol := v.app.volume
	v.app.volumeMu.RUnlock()

	for i := 0; i < n; i++ {
		samples[i][0] *= vol
		samples[i][1] *= vol
	}

	return n, ok
}

func (v *VolumeStreamer) Err() error {
	return v.Streamer.Err()
}

// splitTextSmartly splits text into chunks based on paragraphs and sentences
func splitTextSmartly(text string, maxSize int) []string {
	if len(text) <= maxSize {
		return []string{text}
	}

	var chunks []string
	paragraphs := strings.Split(text, "\n\n")

	currentChunk := ""
	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		// If paragraph fits in current chunk
		if len(currentChunk)+len(para)+2 <= maxSize {
			if currentChunk != "" {
				currentChunk += "\n\n"
			}
			currentChunk += para
		} else {
			// Save current chunk if not empty
			if currentChunk != "" {
				chunks = append(chunks, currentChunk)
				currentChunk = ""
			}

			// If paragraph itself is too large, split by sentences
			if len(para) > maxSize {
				sentences := splitSentences(para)
				for _, sent := range sentences {
					if len(currentChunk)+len(sent)+1 <= maxSize {
						if currentChunk != "" {
							currentChunk += " "
						}
						currentChunk += sent
					} else {
						if currentChunk != "" {
							chunks = append(chunks, currentChunk)
						}
						// If single sentence is too large, force split
						if len(sent) > maxSize {
							for i := 0; i < len(sent); i += maxSize {
								end := i + maxSize
								if end > len(sent) {
									end = len(sent)
								}
								chunks = append(chunks, sent[i:end])
							}
							currentChunk = ""
						} else {
							currentChunk = sent
						}
					}
				}
			} else {
				currentChunk = para
			}
		}
	}

	if currentChunk != "" {
		chunks = append(chunks, currentChunk)
	}

	return chunks
}

// splitSentences splits text by sentence-ending punctuation
func splitSentences(text string) []string {
	re := regexp.MustCompile(`[.!?]+\s+`)
	return re.Split(text, -1)
}

// onReady initializes the system tray menu
func (app *TTSApp) onReady() {
	systray.SetTitle("TTS Player")
	systray.SetTooltip("Text-to-Speech Player")

	//set icon to file tts.ico
	iconData, err := os.ReadFile("tts.ico")
	if err != nil {
		log.Printf("Error loading icon: %v", err)
	} else {
		systray.SetIcon(iconData)
	}

	// Create menu items
	mVolume := systray.AddMenuItem("Volume", "Adjust playback volume")
	mVolumeLow := mVolume.AddSubMenuItem("Low", "Set volume to low")
	mVolumeMed := mVolume.AddSubMenuItem("Medium", "Set volume to medium")
	mVolumeMed.Check()
	mVolumeHigh := mVolume.AddSubMenuItem("High", "Set volume to high")

	systray.AddSeparator()
	mPause := systray.AddMenuItem("Pause", "Pause playback")
	mStop := systray.AddMenuItem("Stop", "Stop and clear queue")
	mReplay := systray.AddMenuItem("Replay", "Replay current chunk")

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	// Handle menu item clicks
	go func() {
		for {
			select {
			case <-mVolumeLow.ClickedCh:
				app.volumeMu.Lock()
				app.volume = 0.2
				app.volumeMu.Unlock()
				mVolumeLow.Check()
				mVolumeMed.Uncheck()
				mVolumeHigh.Uncheck()
				log.Println("Volume set to low")

			case <-mVolumeMed.ClickedCh:
				app.volumeMu.Lock()
				app.volume = 0.5
				app.volumeMu.Unlock()
				mVolumeLow.Uncheck()
				mVolumeMed.Check()
				mVolumeHigh.Uncheck()
				log.Println("Volume set to medium")

			case <-mVolumeHigh.ClickedCh:
				app.volumeMu.Lock()
				app.volume = 1.0
				app.volumeMu.Unlock()
				mVolumeLow.Uncheck()
				mVolumeMed.Uncheck()
				mVolumeHigh.Check()
				log.Println("Volume set to high")

			case <-mPause.ClickedCh:
				app.pauseMu.Lock()
				app.isPaused = !app.isPaused
				if app.isPaused {
					mPause.SetTitle("Resume")
					log.Println("Playback paused")
				} else {
					mPause.SetTitle("Pause")
					app.pauseCond.Broadcast()
					log.Println("Playback resumed")
				}
				app.pauseMu.Unlock()

			case <-mStop.ClickedCh:
				app.stopCurrentMu.Lock()
				app.stopCurrent = true
				app.stopCurrentMu.Unlock()

				app.queueMu.Lock()
				app.queue = make([]QueueItem, 0)
				app.queueMu.Unlock()

				// Resume if paused to allow stop to take effect
				app.pauseMu.Lock()
				if app.isPaused {
					app.isPaused = false
					app.pauseCond.Broadcast()
				}
				app.pauseMu.Unlock()

				speaker.Clear()
				log.Println("Playback stopped and queue cleared")

			case <-mReplay.ClickedCh:
				app.replayMu.Lock()
				app.replayRequest = true
				app.replayMu.Unlock()

				app.stopCurrentMu.Lock()
				app.stopCurrent = true
				app.stopCurrentMu.Unlock()

				speaker.Clear()
				log.Println("Replay requested")

			case <-mQuit.ClickedCh:
				log.Println("Quit requested")
				systray.Quit()
				return
			}
		}
	}()
}

// onExit performs cleanup when the application exits
func (app *TTSApp) onExit() {
	log.Println("Shutting down...")

	// Cancel context to stop worker
	app.cancel()

	// Shutdown HTTP server
	if app.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.server.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}

	// Clear speaker
	if app.speakerInit {
		speaker.Clear()
		speaker.Close()
	}

	log.Println("Shutdown complete")
	os.Exit(0)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// extractMP4Audio attempts to extract audio data from an MP4 container
// It looks for the 'mdat' atom which contains the media data.
// Note: This is a very basic extractor and assumes the mdat atom contains
// the audio stream that can be decoded by the mp3 decoder (e.g. MP3 in MP4).
// It does NOT decode AAC.
func extractMP4Audio(data []byte) ([]byte, error) {
	reader := bytes.NewReader(data)
	for reader.Len() > 8 {
		var size uint32
		var typeBytes [4]byte
		if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
			return nil, err
		}
		if err := binary.Read(reader, binary.BigEndian, &typeBytes); err != nil {
			return nil, err
		}
		atomType := string(typeBytes[:])

		if atomType == "mdat" {
			// Found media data
			payloadSize := int64(size) - 8
			if payloadSize <= 0 {
				// If size is 0, it extends to end of file (handled roughly here)
				// But for mdat it usually has a size.
				// If size was 1, we should have handled it (not implemented for simplicity as mdat usually has size < 4GB for small clips)
				// Let's just read the rest if size is weird or 0
				payload, err := io.ReadAll(reader)
				return payload, err
			}

			payload := make([]byte, payloadSize)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return nil, err
			}
			return payload, nil
		}

		// Skip atom
		if size < 8 {
			// Invalid size or extended size (1) which we don't handle for simplicity in this quick implementation
			// or 0 (end of file)
			if size == 1 {
				// Extended size
				var largeSize uint64
				if err := binary.Read(reader, binary.BigEndian, &largeSize); err != nil {
					return nil, err
				}
				// Skip largeSize - 16
				if _, err := reader.Seek(int64(largeSize)-16, io.SeekCurrent); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("unsupported atom size: %d", size)
		}

		if _, err := reader.Seek(int64(size)-8, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("mdat atom not found")
}
