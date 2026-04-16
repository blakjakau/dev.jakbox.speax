self.addEventListener('push', function(event) {
    if (!event.data) return;

    try {
        const payload = event.data.json();
        const title = payload.title || 'Speax Alert';
        const options = {
            body: payload.body || 'Assistant check-in',
            icon: '/alyx.svg',
            badge: '/alyx.svg',
            data: {
                url: payload.url || '/'
            }
        };

        event.waitUntil(
            self.registration.showNotification(title, options)
        );
    } catch (e) {
        console.error('Push event error:', e);
    }
});

self.addEventListener('notificationclick', function(event) {
    event.notification.close();
    
    const targetUrl = event.notification.data.url;
    
    event.waitUntil(
        clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function(clientList) {
            for (let i = 0; i < clientList.length; i++) {
                let client = clientList[i];
                if ('focus' in client) {
                    client.postMessage({ type: 'NAVIGATE', url: targetUrl });
                    return client.focus();
                }
            }
            if (clients.openWindow) {
                return clients.openWindow(targetUrl);
            }
        })
    );
});
