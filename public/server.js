export class ServerClient {
    constructor(url, callbacks) {
        this.url = url;
        this.socket = null;
        this.callbacks = callbacks;
        this.isGeneratingAi = false;
    }

    connect() {
        this.socket = new WebSocket(this.url);

        this.socket.onopen = () => {
            this.isGeneratingAi = false;
            this.callbacks.onOpen();
        };

        this.socket.onclose = () => {
            this.socket = null;
            this.callbacks.onClose();
        };

        this.socket.onmessage = async (event) => {
            if (event.data instanceof Blob) {
                this.callbacks.onAudioChunk(event.data);
                return;
            }

            const rawText = event.data;

            if (rawText.startsWith("[SETTINGS_SYNC]")) {
                this.callbacks.onSettingsSync(JSON.parse(rawText.substring(15)));
            } else if (rawText.startsWith("[THREADS_SYNC]")) {
                this.callbacks.onThreadsSync(JSON.parse(rawText.substring(14)));
            } else if (rawText.startsWith("[HISTORY]")) {
                this.callbacks.onHistorySync(JSON.parse(rawText.substring(9)));
            } else if (rawText.startsWith("[SUMMARY]")) {
                this.callbacks.onSummarySync(JSON.parse(rawText.substring(9)));
            } else if (rawText.startsWith("[FULL_EXPORT]")) {
                this.callbacks.onFullExportSync(JSON.parse(rawText.substring(13)));
            } else if (rawText.startsWith("[TTS_CHUNK]")) {
                this.callbacks.onTtsChunk(rawText.substring(11));
            } else if (rawText.startsWith("[TOOL_UI_EVENT]")) {
                let eventPayload = null;
                try {
                    eventPayload = JSON.parse(rawText.substring(15));
                } catch (e) {
                    console.error("Failed to parse tool UI event", e);
                }
                if (eventPayload && this.callbacks.onToolUIEvent) {
                    this.callbacks.onToolUIEvent(eventPayload);
                }
            } else if (rawText.trim() === "[AI_START]") {
                this.isGeneratingAi = true;
                this.callbacks.onAiStart();
            } else if (rawText.trim() === "[AI_END]") {
                this.isGeneratingAi = false;
                this.callbacks.onAiEnd();
            } else if (rawText.trim() === "[IGNORED]") {
                this.callbacks.onIgnored();
            } else if (rawText.startsWith("[CHAT]")) {
                this.callbacks.onUserText(rawText.substring(7));
            } else if (this.isGeneratingAi) {
                this.callbacks.onAiText(rawText);
            } else if (rawText.trim() !== "") {
                this.callbacks.onUserText(rawText.trim());
            }
        };
    }

    isOpen() {
        return this.socket && this.socket.readyState === WebSocket.OPEN;
    }

    disconnect() {
        if (this.socket) {
            this.socket.close();
        }
    }

    send(data) {
        if (this.isOpen()) {
            this.socket.send(data);
        }
    }

    sendSettings(settingsObj) {
        this.send(`[SETTINGS]${JSON.stringify(settingsObj)}`);
    }

    sendRestoreClientThreads(payloadObj) {
        this.send(`[RESTORE_CLIENT_THREADS]${JSON.stringify(payloadObj)}`);
    }

    sendRequestSync() {
        this.send("[REQUEST_SYNC]");
    }

    sendRequestFullExport() {
        this.send("[REQUEST_FULL_EXPORT]");
    }

    sendNewThread(name) {
        this.send(`[NEW_THREAD]:${name}`);
    }

    sendSwitchThread(id) {
        this.send(`[SWITCH_THREAD]:${id}`);
    }

    sendRenameThread(name) {
        this.send(`[RENAME_THREAD]:${name}`);
    }

    sendDeleteThread(id) {
        this.send(`[DELETE_THREAD]:${id}`);
    }

    sendDeleteMsg(idx) {
        this.send(`[DELETE_MSG]:${idx}`);
    }

    sendClearHistory() {
        this.send("[CLEAR_HISTORY]");
    }

    sendRebuildSummary() {
        this.send("[REBUILD_SUMMARY]");
    }
}
