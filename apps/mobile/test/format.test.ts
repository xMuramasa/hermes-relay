import { expect, test } from "bun:test";
import { formatDuration, isWritable, relativeTime, sessionTitle, sourceLabel } from "../src/lib/format";

test("session labels", () => {
  expect(sessionTitle({ title: null, preview: null })).toBe("New chat");
  expect(sessionTitle({ title: "  ", preview: "one two three four five six seven eight nine" })).toBe("one two three four five six seven eight");
  expect(sourceLabel("api_server")).toBe("app");
  expect(sourceLabel("telegram")).toBe("Telegram");
  expect(sourceLabel("some_new_surface")).toBe("some new surface");
  expect(isWritable({ source: "api_server" })).toBe(true);
  expect(isWritable({ source: "telegram" })).toBe(false);
});

test("relative time and durations", () => {
  const now = 1_792_801_000_000;
  expect(relativeTime(null, now)).toBe("");
  expect(relativeTime(now / 1000 - 30, now)).toBe("just now");
  expect(relativeTime(now / 1000 - 600, now)).toBe("10 min ago");
  expect(relativeTime(now / 1000 - 3 * 3600, now)).toBe("3 h ago");
  expect(formatDuration(1.34)).toBe("1.3 s");
  expect(formatDuration(42.4)).toBe("42 s");
  expect(formatDuration(undefined)).toBe("");
});
