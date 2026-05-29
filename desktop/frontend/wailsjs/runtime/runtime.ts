// Stub Wails runtime for standalone npm build.
// Wails replaces this with the real runtime during wails build/dev.

export function EventsOn(
  _eventName: string,
  _callback: (...data: unknown[]) => void
): () => void {
  return () => {}
}

export function EventsOff(..._eventNames: string[]): void {}

export function EventsOnce(
  _eventName: string,
  _callback: (...data: unknown[]) => void
): void {}

export function EventsEmit(_eventName: string, ..._data: unknown[]): void {}
