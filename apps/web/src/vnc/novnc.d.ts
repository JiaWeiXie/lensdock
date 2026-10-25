/**
 * Minimal typings for the public noVNC RFB API (see
 * node_modules/@novnc/novnc/docs/API.md). Only members documented there are
 * declared; private underscore fields are intentionally absent.
 */
declare module '@novnc/novnc' {
  export interface RFBOptions {
    shared?: boolean;
    credentials?: { username?: string; password?: string; target?: string };
    repeaterID?: string;
    wsProtocols?: string[];
  }

  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, urlOrChannel: string | WebSocket | RTCDataChannel, options?: RFBOptions);

    viewOnly: boolean;
    focusOnClick: boolean;
    clipViewport: boolean;
    dragViewport: boolean;
    scaleViewport: boolean;
    resizeSession: boolean;
    showDotCursor: boolean;
    background: string;
    qualityLevel: number;
    compressionLevel: number;
    readonly capabilities: { power: boolean };

    disconnect(): void;
    sendCredentials(credentials: { username?: string; password?: string; target?: string }): void;
    sendKey(keysym: number, code: string | null, down?: boolean): void;
    sendCtrlAltDel(): void;
    focus(options?: FocusOptions): void;
    blur(): void;
    toBlob(callback: BlobCallback, type?: string, quality?: number): void;
  }
}
