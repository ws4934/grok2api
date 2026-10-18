// Type-only imports keep this module loadable by node --test.
import type { VoiceInfo } from "./creative-console-api";

const grokDefaultVoice: VoiceInfo = { voiceId: "eve", name: "eve" };

/**
 * Resolves the TTS voice for the active model: the chosen voice when the model's list offers it,
 * otherwise the list's first voice. Only Grok has a known default to offer while its list is empty
 * or failed to load; other providers get no options and send no voice, so the upstream uses the
 * account's own voice.
 */
export function resolveVoice(voices: VoiceInfo[], choice: string, grok: boolean): { options: VoiceInfo[]; voiceId: string } {
  const options = voices.length > 0 ? voices : grok ? [grokDefaultVoice] : [];
  return { options, voiceId: options.some((voice) => voice.voiceId === choice) ? choice : options[0]?.voiceId ?? "" };
}
