import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  checkPriceInput,
  decodeRoutePrice,
  distinctRoutePrices,
  formatPriceUSD,
  priceComponents,
  priceHeadline,
  priceOperation,
} from "./model-price.ts";

describe("price operations", () => {
  it("maps route capabilities to billing operations", () => {
    assert.equal(priceOperation("responses"), "text");
    assert.equal(priceOperation("chat"), "text");
    assert.equal(priceOperation("image"), "image");
    assert.equal(priceOperation("image_edit"), "image_edit");
    assert.equal(priceOperation("video"), "video");
    assert.equal(priceOperation("tts"), "tts");
    assert.equal(priceOperation("stt"), "stt");
    assert.equal(priceOperation("realtime"), undefined);
  });

  it("lists required and optional components like the server", () => {
    assert.deepEqual(priceComponents("text"), [
      { component: "inputTokenUSDPerMillion", required: true },
      { component: "cachedInputTokenUSDPerMillion", required: false },
      { component: "outputTokenUSDPerMillion", required: true },
    ]);
    assert.deepEqual(priceComponents("image_edit").map(({ required }) => required), [true, true]);
    assert.deepEqual(priceComponents("video"), [
      { component: "inputImageUSD", required: false },
      { component: "outputSecondUSD", required: true },
    ]);
    assert.deepEqual(priceComponents("tts"), [{ component: "characterUSDPerMillion", required: true }]);
    assert.deepEqual(priceComponents("stt"), [{ component: "outputSecondUSD", required: true }]);
  });
});

describe("checkPriceInput", () => {
  it("returns only the operation's non-empty components, trimmed", () => {
    const result = checkPriceInput("text", { inputTokenUSDPerMillion: " 1.25 ", outputTokenUSDPerMillion: "10", cachedInputTokenUSDPerMillion: "", outputImageUSD: "1" });
    assert.deepEqual(result, { errors: {}, components: { inputTokenUSDPerMillion: "1.25", outputTokenUSDPerMillion: "10" } });
  });

  it("requires the operation's required components", () => {
    assert.deepEqual(checkPriceInput("text", { inputTokenUSDPerMillion: "1" }).errors, { outputTokenUSDPerMillion: "required" });
    assert.deepEqual(checkPriceInput("image_edit", { outputImageUSD: "0.04" }).errors, { inputImageUSD: "required" });
    assert.deepEqual(checkPriceInput("video", { outputSecondUSD: "0.1" }).errors, {});
  });

  it("rejects values the server cannot store exactly", () => {
    const tts = (value: string) => checkPriceInput("tts", { characterUSDPerMillion: value }).errors.characterUSDPerMillion;
    const image = (value: string) => checkPriceInput("image", { outputImageUSD: value }).errors.outputImageUSD;
    assert.equal(tts("0.0001"), undefined);
    assert.equal(tts("1.50000"), undefined);
    assert.equal(tts("0.00001"), "precision");
    assert.equal(tts("10000"), undefined);
    assert.equal(tts("10000.0001"), "limit");
    assert.equal(tts("99999999999999999999"), "limit");
    assert.equal(image("0.0000000001"), undefined);
    assert.equal(image("0.00000000001"), "precision");
    assert.equal(image("1000.0000000000"), undefined);
    assert.equal(image("1000.0000000001"), "limit");
    for (const value of ["-1", "abc", "1e-4", ".5", "1.", "NaN"]) assert.equal(image(value), "invalid", value);
  });
});

describe("price display", () => {
  it("formats exact USD strings with grouping", () => {
    assert.equal(formatPriceUSD("2"), "$2");
    assert.equal(formatPriceUSD("0.04"), "$0.04");
    assert.equal(formatPriceUSD("1000"), "$1,000");
    assert.equal(formatPriceUSD("10000.0001"), "$10,000.0001");
  });

  it("summarises the main rates with their unit", () => {
    assert.deepEqual(priceHeadline("text", { inputTokenUSDPerMillion: "1.25", cachedInputTokenUSDPerMillion: "0.2", outputTokenUSDPerMillion: "2.5" }), { values: ["$1.25", "$2.5"], unit: "million_tokens" });
    assert.deepEqual(priceHeadline("image_edit", { inputImageUSD: "0.01", outputImageUSD: "0.04" }), { values: ["$0.04"], unit: "image" });
    assert.deepEqual(priceHeadline("tts", { characterUSDPerMillion: "15" }), { values: ["$15"], unit: "million_characters" });
    assert.deepEqual(priceHeadline("video", {}), { values: [], unit: "second" });
  });
});

describe("decodeRoutePrice", () => {
  it("keeps recognised fields", () => {
    assert.deepEqual(decodeRoutePrice({ source: "custom", id: "12", pricingModel: "gpt-5", operation: "text", inputTokenUSDPerMillion: "1.25", outputTokenUSDPerMillion: "10", cachedInputTokenUSDPerMillion: "1.25" }), {
      source: "custom", id: "12", pricingModel: "gpt-5", operation: "text", inputTokenUSDPerMillion: "1.25", outputTokenUSDPerMillion: "10", cachedInputTokenUSDPerMillion: "1.25",
    });
    assert.deepEqual(decodeRoutePrice({ source: "none" }), { source: "none" });
  });

  it("drops malformed parts instead of failing", () => {
    assert.equal(decodeRoutePrice(undefined), undefined);
    assert.equal(decodeRoutePrice("official"), undefined);
    assert.equal(decodeRoutePrice([]), undefined);
    assert.equal(decodeRoutePrice({ source: "tiered" }), undefined);
    assert.deepEqual(decodeRoutePrice({ source: "official", id: 7, pricingModel: "", operation: "audio", outputImageUSD: -1, outputSecondUSD: "1e3", characterUSDPerMillion: "15", extra: true }), {
      source: "official", id: "7", characterUSDPerMillion: "15",
    });
  });
});

describe("distinctRoutePrices", () => {
  it("lists each billed price once and skips unpriceable routes", () => {
    const text = { source: "official" as const, pricingModel: "grok-4.3" };
    const routes = [
      { capability: "chat" as const, price: text },
      { capability: "responses" as const, price: text },
      { capability: "image" as const, price: { source: "none" as const, pricingModel: "grok-4.3" } },
      { capability: "realtime" as const, price: { source: "none" as const } },
      { capability: "tts" as const },
    ];
    assert.deepEqual(distinctRoutePrices(routes).map(({ route, operation, pricingModel }) => [route.capability, operation, pricingModel]), [
      ["chat", "text", "grok-4.3"],
      ["image", "image", "grok-4.3"],
    ]);
  });
});
