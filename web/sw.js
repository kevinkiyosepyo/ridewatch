/* RideWatch service worker: shows push alerts and routes clicks to the stop page. */

self.addEventListener('push', event => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    data = { body: event.data ? event.data.text() : '' };
  }
  event.waitUntil(
    self.registration.showNotification(data.title || 'RideWatch', {
      body: data.body || '',
      data: { url: data.url || '/' },
    })
  );
});

self.addEventListener('notificationclick', event => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || '/';
  const target = new URL(url, self.location.origin);
  event.waitUntil(
    clients.matchAll({ type: 'window', includeUncontrolled: true }).then(list => {
      for (const client of list) {
        if (new URL(client.url).pathname === target.pathname && 'focus' in client) {
          return client.focus();
        }
      }
      return clients.openWindow(target.href);
    })
  );
});
