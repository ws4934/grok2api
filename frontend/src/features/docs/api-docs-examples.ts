import type { ModelRouteDTO } from "@/entities/model/types";

// Dependency-free so node --test can load it.

export type ExampleLanguage = "curl" | "python" | "javascript";
export type Method = "GET" | "POST";
type Capability = ModelRouteDTO["capability"];

export type FieldDefinition = {
  name: string;
  required?: boolean;
  descriptionKey: string;
};

export type Example = {
  request: (model: string) => Record<string, unknown> | undefined;
  response: Record<string, unknown>;
  // Sent as multipart/form-data; the "file" field names a local file to upload.
  multipart?: boolean;
};

export const exampleVariants = ["basic", "tools", "search"] as const;
export type ExampleVariant = (typeof exampleVariants)[number];

export type EndpointDefinition = Example & {
  key: string;
  category: string;
  title: string;
  method: Method;
  path: string;
  descriptionKey: string;
  capabilities: Capability[];
  // Lists only models of these providers, for endpoints a single provider serves.
  providers?: string[];
  fields: FieldDefinition[];
  noteKeys: string[];
  // Function-tool and web-search examples next to the basic one; the same for every provider.
  variants?: Partial<Record<Exclude<ExampleVariant, "basic">, Example>>;
  // Replaces parts of the basic example, and the notes, when the selected model is served by ChatGPT.
  chatgpt?: Partial<Example> & { noteKeys?: string[] };
};

export const exampleLanguages: ExampleLanguage[] = ["curl", "python", "javascript"];

export function availableVariants(definition: EndpointDefinition): ExampleVariant[] {
  return exampleVariants.filter((variant) => variant === "basic" || definition.variants?.[variant]);
}

// resolveExample picks the example for a variant and the notes for the selected model's provider.
export function resolveExample(definition: EndpointDefinition, variant: ExampleVariant, provider?: string): { example: Example; noteKeys: string[] } {
  const chatgpt = provider === "chatgpt" ? definition.chatgpt : undefined;
  const example = (variant === "basic" ? undefined : definition.variants?.[variant]) ?? {
    request: chatgpt?.request ?? definition.request,
    response: chatgpt?.response ?? definition.response,
    multipart: chatgpt?.multipart ?? definition.multipart,
  };
  return { example, noteKeys: chatgpt?.noteKeys ?? definition.noteKeys };
}

export function createExamples(definition: Pick<EndpointDefinition, "key" | "method" | "path">, example: Example, baseUrl: string, model: string): Record<ExampleLanguage, string> {
  const request = example.request(model);
  const path = definition.path
    .replace("{request_id}", "video_example")
    .replace("{response_id}", "resp_img_example");
  const url = definition.key === "voice/realtime"
    ? `${baseUrl}${path}?model=${encodeURIComponent(model)}`
    : definition.key === "voice/voices"
      ? `${baseUrl}${path}?model=${encodeURIComponent(model)}`
      : `${baseUrl}${path}`;
  if (example.multipart && request) return createMultipartExamples(definition.method, url, request);
  const messageHeaders = definition.key === "chat/messages";
  const curlHeaders = messageHeaders
    ? [
        '  -H "x-api-key: $GROK2API_API_KEY"',
        '  -H "anthropic-version: 2023-06-01"',
        '  -H "Content-Type: application/json"',
      ].join(" \\\n")
    : [
        '  -H "Authorization: Bearer $GROK2API_API_KEY"',
        '  -H "Content-Type: application/json"',
      ].join(" \\\n");
  const curlBody = request ? ` \\\n  -d '${JSON.stringify(request, null, 2)}'` : "";
  const headers = messageHeaders
    ? { "x-api-key": "g2a_your_api_key", "anthropic-version": "2023-06-01", "Content-Type": "application/json" }
    : { Authorization: "Bearer g2a_your_api_key", "Content-Type": "application/json" };
  const pythonImports = request ? "import json\nimport requests" : "import requests";
  const pythonPayload = request ? `\n\npayload = json.loads(r'''${JSON.stringify(request, null, 2)}''')` : "";
  const pythonBody = request ? ",\n    json=payload" : "";
  const javascriptBody = request ? `,\n  body: JSON.stringify(${JSON.stringify(request, null, 2)})` : "";
  return {
    curl: `export GROK2API_API_KEY="g2a_your_api_key"\n\ncurl -X ${definition.method} "${url}" \\\n${curlHeaders}${curlBody}`,
    python: `${pythonImports}${pythonPayload}\n\nresponse = requests.${definition.method.toLowerCase()}(\n    "${url}",\n    headers=${JSON.stringify(headers, null, 2)}${pythonBody}\n)\nresponse.raise_for_status()\nprint(response.json())`,
    javascript: `const response = await fetch("${url}", {\n  method: "${definition.method}",\n  headers: ${JSON.stringify(headers, null, 2)}${javascriptBody}\n});\n\nif (!response.ok) throw new Error(await response.text());\nconsole.log(await response.json());`,
  };
}

// Multipart uploads let the client set the boundary, so no Content-Type header is sent.
function createMultipartExamples(method: Method, url: string, request: Record<string, unknown>): Record<ExampleLanguage, string> {
  const { file, ...rest } = request;
  const fileName = String(file);
  const fields = Object.fromEntries(Object.entries(rest).map(([name, value]) => [name, String(value)]));
  const curlFields = Object.entries(fields).map(([name, value]) => ` \\\n  -F "${name}=${value}"`).join("");
  const formFields = Object.entries(fields).map(([name, value]) => `form.append(${JSON.stringify(name)}, ${JSON.stringify(value)});\n`).join("");
  return {
    curl: `export GROK2API_API_KEY="g2a_your_api_key"\n\ncurl -X ${method} "${url}" \\\n  -H "Authorization: Bearer $GROK2API_API_KEY" \\\n  -F "file=@${fileName}"${curlFields}`,
    python: `import requests\n\nwith open("${fileName}", "rb") as file:\n    response = requests.${method.toLowerCase()}(\n        "${url}",\n        headers={"Authorization": "Bearer g2a_your_api_key"},\n        data=${JSON.stringify(fields)},\n        files={"file": file},\n    )\nresponse.raise_for_status()\nprint(response.json())`,
    javascript: `import { openAsBlob } from "node:fs";\n\nconst form = new FormData();\nform.append("file", await openAsBlob("${fileName}"), "${fileName}");\n${formFields}\nconst response = await fetch("${url}", {\n  method: "${method}",\n  headers: { Authorization: "Bearer g2a_your_api_key" },\n  body: form\n});\n\nif (!response.ok) throw new Error(await response.text());\nconsole.log(await response.json());`,
  };
}
