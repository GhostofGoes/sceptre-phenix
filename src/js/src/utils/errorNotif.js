import { usePhenixStore } from '@/store.js';
import { openNotification } from '@/utils/notify.js';

export async function useErrorNotification(error) {
  // requests are cancelled on purpose when the user leaves a page
  if (error?.code === 'ERR_CANCELED') return;

  let message;
  console.warn('Error', error);
  if (!('response' in error)) {
    message = error.message;
  } else if (error.response.headers.get('content-type') == 'application/json') {
    let msg = error.response.data;
    message = `<h2><b>Error:</b> ${msg.message}</h2>`;

    if (msg.cause) {
      let cause = msg.cause.replace(/\n/g, '<br>').replace(/\t/g, '&emsp;');
      message = `${message}<br><b>Cause:</b> ${cause}`;
    }
  } else if (error.response.data) {
    // if the error is for an invalid token, log the user out
    if (
      error.response.status === 401 &&
      String(error.response.data).toLowerCase().includes('invalid')
    ) {
      usePhenixStore().logout();
      message = 'Token was invalid. Logging out';
    } else {
      message = `<b>Error:</b> ${error.response.data}`;
    }
  } else {
    message = `<b>Unknown Error Occurred: ${error.response.statusText}</b>`;
  }

  showErrorNotification(message);
}

const escapeHTML = (text) =>
  String(text)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');

// Shows an error that did not come from a request, such as one reported over
// the websocket. title and detail are plain text.
export function showError(title, detail) {
  let message = `<h2><b>Error:</b> ${escapeHTML(title)}</h2>`;
  if (detail) {
    message += `<br><b>Cause:</b> ${escapeHTML(detail).replace(/\n/g, '<br>')}`;
  }
  showErrorNotification(message);
}

function showErrorNotification(message) {
  openNotification({
    type: 'is-danger',
    hasIcon: true,
    position: 'is-top',
    indefinite: true,
    message: message,
  });
}
