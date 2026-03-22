/**
 * Speax Tool Client SDK
 * 
 * Provides a standardized way for external web apps (like dev.jakbox.code) to register
 * actionable capabilities with the dev.jakbox.speaks orchestration server via WebSockets.
 */

export class SpeaxToolClient {
    constructor(serverUrl, toolName) {
        this.serverUrl = serverUrl;
        this.wsUrl = serverUrl.replace('http', 'ws') + '/ws?client=tool&device=' + encodeURIComponent(toolName);
        this.toolName = toolName;
        this.socket = null;
        this.reconnectTimer = null;
        this.reconnectAttempts = 0;
        this.actions = new Map(); // name -> { description, schema, executeCallback }
        
        // Event hooks for host applications
        this.onConnect = null;
        this.onDisconnect = null;
        this.onError = null;

        // Try to find the session cookie locally to prove authentication,
        // although standard WebSocket connections will automatically send cookies for the same domain.
        this.sessionId = this._getCookie('speax_session');
    }

    _getCookie(name) {
        const value = `; ${document.cookie}`;
        const parts = value.split(`; ${name}=`);
        if (parts.length === 2) return parts.pop().split(';').shift();
        return null;
    }

    /**
     * Register a capability that the LLM can call.
     * @param {string} name - Action identifier (e.g. "open_buffer")
     * @param {string} description - Human-readable explanation for the AI
     * @param {Object} schema - JSON Schema representing the required payload
     * @param {Function} executeCallback - Async function called when LLM triggers this action
     */
    registerAction(name, description, schema, executeCallback) {
        this.actions.set(name, {
            name,
            description,
            schema,
            executeCallback
        });

        // If we are already connected, immediately tell the server about this new action.
        if (this.socket && this.socket.readyState === WebSocket.OPEN) {
            this._syncActions();
        }
    }

    /**
     * Connect the WebSocket to the Speax Go orchestration server.
     */
    connect() {
        if (this.reconnectTimer) {
            clearTimeout(this.reconnectTimer);
            this.reconnectTimer = null;
        }

        if (!this.sessionId) {
            console.warn(`[SpeaxToolClient] No speax_session cookie found. Connection might fail if unauthenticated.`);
        }

        console.log(`[SpeaxToolClient ${this.toolName}] Connecting to ${this.wsUrl}...`);
        this.socket = new WebSocket(this.wsUrl);

        this.socket.onopen = () => {
            console.log(`[SpeaxToolClient ${this.toolName}] Connected.`);
            this.reconnectAttempts = 0;
            if (this.reconnectTimer) {
                clearTimeout(this.reconnectTimer);
                this.reconnectTimer = null;
            }
            this._syncActions();
            if (typeof this.onConnect === 'function') this.onConnect();
        };

        this.socket.onmessage = async (event) => {
            // Check if the message is a tool execution request
            if (typeof event.data === 'string' && event.data.startsWith('[TOOL_EXECUTE]')) {
                try {
                    const payloadStr = event.data.replace('[TOOL_EXECUTE]', '');
                    const request = JSON.parse(payloadStr);
                    await this._handleExecutionRequest(request);
                } catch (err) {
                    console.error(`[SpeaxToolClient ${this.toolName}] Failed to parse execution request:`, err);
                }
            }
        };

        this.socket.onclose = () => {
            const delay = Math.min(Math.pow(2, this.reconnectAttempts + 1) * 1000, 10000);
            console.log(`[SpeaxToolClient ${this.toolName}] Disconnected. Reconnecting in ${delay/1000}s...`);
            
            this.socket = null;
            if (typeof this.onDisconnect === 'function') this.onDisconnect();
            
            this.reconnectAttempts++;
            this.reconnectTimer = setTimeout(() => this.connect(), delay);
        };

        this.socket.onerror = (err) => {
            console.error(`[SpeaxToolClient ${this.toolName}] WS Error:`, err);
            if (typeof this.onError === 'function') this.onError(err);
            // onclose will fire right after this and handle reconnection
        };
    }

    _syncActions() {
        const actionPayloads = Array.from(this.actions.values()).map(a => ({
            name: a.name,
            description: a.description,
            schema: a.schema
        }));

        const msg = JSON.stringify({
            toolName: this.toolName,
            actions: actionPayloads
        });

        this.socket.send(`[TOOL_REGISTER]${msg}`);
        console.log(`[SpeaxToolClient ${this.toolName}] Synced ${actionPayloads.length} actions.`);
    }

    /**
     * Send an unsolicited event to the orchestrator.
     * @param {string} eventName - Type of event (e.g. "TIMER_EXPIRED")
     * @param {Object} payload - Data associated with the event
     */
    sendEvent(eventName, payload) {
        if (this.socket && this.socket.readyState === WebSocket.OPEN) {
            const msg = JSON.stringify({ eventName, toolName: this.toolName, payload });
            this.socket.send(`[TOOL_EVENT]${msg}`);
        } else {
            console.error(`[SpeaxToolClient ${this.toolName}] Cannot send event, socket is closed!`);
        }
    }

    async _handleExecutionRequest(request) {
        const { executionId, actionName, params } = request;
        console.log(`[SpeaxToolClient ${this.toolName}] Received execution request ${executionId} for action: ${actionName}`, params);

        const action = this.actions.get(actionName);
        let resultPayload = {
            executionId,
            toolName: this.toolName,
            actionName,
            status: "success",
            data: null,
            error: null
        };

        if (!action) {
            resultPayload.status = "error";
            resultPayload.error = `Action ${actionName} is not registered on this tool client.`;
        } else {
            try {
                // Execute the callback registered by the host application
                const data = await action.executeCallback(params);
                resultPayload.data = data;
            } catch (err) {
                console.error(`[SpeaxToolClient ${this.toolName}] Action ${actionName} failed:`, err);
                resultPayload.status = "error";
                resultPayload.error = err.message || String(err);
            }
        }

        // Send the result back to the Go Orchestrator
        if (this.socket && this.socket.readyState === WebSocket.OPEN) {
            this.socket.send(`[TOOL_RESULT]${JSON.stringify(resultPayload)}`);
            console.log(`[SpeaxToolClient ${this.toolName}] Sent result for ${executionId}:`, resultPayload.status);
        } else {
            console.error(`[SpeaxToolClient ${this.toolName}] Cannot send result, socket is closed!`);
        }
    }
}
