import { Compass, Plug, SquareTerminal, Webhook, type LucideIcon } from "lucide-react";

const grokIcons: ReadonlyMap<string, LucideIcon> = new Map([["grok_build", SquareTerminal], ["grok_web", Compass], ["grok_console", Webhook]]);

export function providerIcon(id: string): LucideIcon {
  return grokIcons.get(id) ?? Plug;
}
