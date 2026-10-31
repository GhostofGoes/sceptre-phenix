# Troubleshooting

## Error Codes and Diagram Issues

See [Error Codes](error-codes.md) for error codes and diagram issue codes.

## Messages

These are references for different messages a user may get from Builder.

### Draft Save state

| Save state | What it means | What to do |
|---|---|---|
| All changes saved | The server has all changes. | Nothing. |
| Saving changes, Saving 2 changes | Builder sends changes. | Wait. |
| 2 unsaved changes | Changes wait. | Wait, or press **Save now**. |
| Offline: 1 change kept on this device. Saving retries automatically. | The server cannot be reached, or did not answer in time. This browser keeps the changes. | Continue to work. See [Working offline](drafts.md#working-offline). |
| Offline: 2 changes not stored anywhere yet. Keep this tab open. Saving retries automatically. | The server cannot be reached, and the browser cannot store the changes. | Keep the tab open until the save state says All changes saved. |
| Offline: no unsaved changes | The server cannot be reached, and no changes wait. | Nothing. |
| 1 change not saved yet. Choose which changes to save. | Another tab of this browser has changes to this draft. | See [Same draft in multiple tabs](drafts.md#same-draft-in-multiple-tabs). |
| Merging your changes with the server version… | Someone saved a newer version first. Builder merges it with your changes. | Wait. See [Draft merging](drafts.md#draft-merging). |
| This draft changed on the server | Someone saved a newer version first. Some changes clash, or merging is not available. | See [Draft merging](drafts.md#draft-merging). |
| Not saved: you chose another tab's changes | You chose the changes of another tab. | Nothing. Builder keeps the changes of this tab as a new draft. |
| Could not save: someone deleted this draft from the server. Select Download to keep a copy of the diagram. | Another user deleted the draft. | Select **Download** (see [Downloading](import-upload-download.md#downloading)). Upload the file to make a new draft. |
| You cannot save changes to this draft | Your role no longer includes the Builder. | Ask an administrator for the Builder permission (see [Permissions](administration.md#permissions)). Select **Download** to keep a copy. |
| Could not save your last change. … When you fix the diagram, the Builder saves it again. | The server refused the diagram, for example for two devices with one hostname. The message names the problem. | Fix it (see [Checks and warnings](editor.md#checks-and-warnings)). The next change saves the diagram. |
| Could not save your changes. … Saving retries automatically. | The server refused the save for a different reason, which the message names. | Wait, or select **Retry saving**. |

**Retry saving** shows when the browser is offline, or when a new try can
fix a failed save. It sends the waiting changes immediately.
