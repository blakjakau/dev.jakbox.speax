/**
 * Shared JS utilities for Speax Tools
 */

export function checkAuth() {
    return document.cookie.includes('speax_session=');
}

export function handleAuthRedirect() {
    if (!checkAuth()) {
        window.location.href = '/auth/login?client=tool';
        return false;
    }
    return true;
}

export function setupStatusIndicator(client, dotId, textId) {
    const dot = document.getElementById(dotId);
    const text = document.getElementById(textId);
    
    if (!dot || !text) return;

    client.onConnect = () => {
        dot.classList.add('online');
        text.textContent = 'Connected';
        console.log('Speax connection established.');
    };

    client.onDisconnect = () => {
        dot.classList.remove('online');
        text.textContent = 'Disconnected';
        console.log('Speax connection lost.');
    };
}

export function createLogger(containerId) {
    const container = document.getElementById(containerId);
    if (!container) return { log: () => {} };

    return {
        log: (msg, type = 'info') => {
            const div = document.createElement('div');
            div.className = `log-entry log-${type}`;
            const time = new Date().toLocaleTimeString([], { 
                hour: '2-digit', 
                minute: '2-digit', 
                second: '2-digit' 
            });
            div.innerHTML = `<span class="log-time">${time}</span> ${msg}`;
            container.appendChild(div);
            container.scrollTop = container.scrollHeight;
            
            // Limit logs to last 100 entries
            while (container.children.length > 100) {
                container.removeChild(container.firstChild);
            }
        }
    };
}

/**
 * Utility to notify the parent container (tools.html if nested)
 * about tool events or status changes.
 */
export function notifyParent(event, data) {
    if (window.parent && window.parent !== window) {
        window.parent.postMessage({ type: 'TOOL_EVENT', event, data }, '*');
    }
}
