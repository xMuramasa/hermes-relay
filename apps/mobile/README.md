# Trismegistus app (`apps/mobile`)

Expo SDK 58 (React Native 0.88), Expo Router, TypeScript strict, NativeWind 4. Runs in **Expo Go 58** on Android;
no native code, no prebuild. Phase 2a: pair, session list, chat.

## Run it on the phone

1. Install **Expo Go** from the Play Store (SDK 58 is there since 2026-10-19).
2. On optiplex or the Mac:

   ```sh
   cd apps/mobile
   bun install
   REACT_NATIVE_PACKAGER_HOSTNAME=<host IP reachable from the phone> bun run start
   ```

   Scan the QR code it prints with Expo Go.
3. Pair: on optiplex run

   ```sh
   docker exec -it trismegistus-relay trismegistus-relay pair --name <device>
   ```

   and scan that QR code from the app's pair screen (or type the relay URL and the code by hand). The code is valid
   for 10 minutes and works once.

The phone must reach both the dev server (port 8081 on the host above) and `https://hermes.lab.x-muramasa.dev`
(LAN or tailnet).

## Checks

```sh
bun run check       # typecheck + bun test
bun run typecheck   # tsc --noEmit
bun test            # test/*.test.ts
bun run doctor      # expo-doctor
bunx expo export --platform android --output-dir dist && rm -rf dist   # bundle builds
```

The tests need no phone and no network: they read `../../protocol/` (schema and examples) and
`../../probe/fixtures/` by path, and use a fake `fetch` and a fake WebSocket (`test/fakes.ts`).

## Layout

- `src/app/`: routes. `_layout.tsx` (fonts, theme, pair/sessions guard), `pair.tsx`, `index.tsx` (sessions),
  `chat/[id].tsx`.
- `src/lib/` (plain TypeScript, no React):
  - `protocol.ts`: message types, `decodeServerMessage` (checks every relay frame against the schema's rules),
    builders for phone messages.
  - `pairing.ts`: QR payload parsing, code/URL checks, `redeem` (`POST /api/pair`).
  - `api.ts`: REST client (Bearer token, 15 s timeout, errors as protocol codes), response parsing and bounds.
  - `socket.ts`: the one WebSocket (auth → ready → subscribe, ping every 20 s, no auto-reconnect yet).
  - `chat.ts`: the chat reducer (history + live events).
  - `format.ts`, `theme.ts`, `utils.ts`.
- `src/state/app.ts`: zustand store; pairing in `expo-secure-store`.
- `src/components/`: `ui/` (react-native-reusables copies: button, input, text, card, badge, skeleton,
  alert-dialog), `markdown.tsx`, `chat-items.tsx`, `sigil.tsx`, `connection-dot.tsx`.
- `src/global.css`: theme tokens (CSS variables, light and dark follow the system); `tailwind.config.js` maps them.
- `test/`: `bun test` suites and fakes.
