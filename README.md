# TTS Player Server

This is a simple Text-to-Speech (TTS) server that runs as a system tray application. It receives text via an HTTP endpoint, queues it, and plays it back as audio using a local TTS engine.

<img width="1024" height="1024" alt="tts" src="https://github.com/user-attachments/assets/d287869c-6a54-46e1-83c4-fc19fbd24635" />

## Features

- **HTTP Endpoint**: Queue text for playback by sending POST requests to `/queue`.
- **System Tray Control**: Manage playback directly from a system tray icon.
- **Playback Management**:
    - Pause and Resume audio playback.
    - Stop the current audio and clear the entire queue.
    - Replay the last played audio chunk.
- **Volume Control**: Adjust volume levels (Low, Medium, High).
- **Smart Text Chunking**: Intelligently splits long texts into smaller chunks to handle API limits and ensure smooth playback.
- **Background Worker**: Processes the text queue in the background.

## Prerequisites

Before running this application, you need:

1.  **Go**: Ensure you have Go installed on your system.
2.  **Kokoro TTS API**: This application is designed to work with a [Kokoro-FastAPI](https://github.com/remsky/Kokoro-FastAPI) accessible at `http://localhost:8880/v1/audio/speech`. You must have this service running.

## Installation and Running

1.  **Clone the repository:**
    ```sh
    git clone <repository-url>
    cd tts-player-server
    ```

2.  **Install dependencies:**
    ```sh
    go mod tidy
    ```

3.  **Run the application:**
    ```sh
    go run main.go
    ```

The application will start, and you will see a new icon in your system tray.

## Release Binary

You can download the pre-built binary for your platform from the [releases page](https://github.com/rpfilomeno/tts-player-server/releases).

## API Usage

To queue text for playback, send a POST request with a JSON payload to the `/queue` endpoint.

**Endpoint**: `POST /queue`
**Address**: `http://localhost:50059`

### Example using cURL

```sh
curl -X POST -H "Content-Type: application/json" -d "{\"text\": \"Hello, world! This is a test.\"}" http://localhost:50059/queue
```

## System Tray Controls

Right-clicking the tray icon opens a menu with the following options:

- **Volume**:
    - **Low**: Sets playback volume to 20%.
    - **Medium**: Sets playback volume to 50% (Default).
    - **High**: Sets playback volume to 100%.
- **Pause / Resume**: Toggles pausing and resuming of the current audio playback.
- **Stop**: Immediately stops the current playback and clears all items from the queue.
- **Replay**: Stops the current playback and replays the last piece of audio that was generated.
- **Quit**: Shuts down the server and closes the application.
