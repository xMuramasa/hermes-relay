import { CameraView, useCameraPermissions, type BarcodeScanningResult } from "expo-camera";
import { useRef, useState } from "react";
import { ActivityIndicator, KeyboardAvoidingView, ScrollView, View } from "react-native";
import { SafeAreaView } from "react-native-safe-area-context";
import { Sigil } from "@/components/sigil";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Text } from "@/components/ui/text";
import { normalizeCode, normalizeUrl, pairingErrorText, parsePairingPayload, redeem } from "@/lib/pairing";
import { usePalette } from "@/lib/theme";
import { useApp } from "@/state/app";

export default function PairScreen() {
  const [permission, requestPermission] = useCameraPermissions();
  const [scanning, setScanning] = useState(false);
  const [manual, setManual] = useState(false);
  const [url, setUrl] = useState("https://");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const handled = useRef(false); // the scanner fires many times per second
  const p = usePalette();

  async function submit(relayUrl: string, pairCode: string) {
    setBusy(true);
    setError(null);
    try {
      const creds = await redeem(relayUrl, pairCode);
      await useApp.getState().pair(creds); // the layout's guard then shows the sessions list
    } catch (e) {
      setError(pairingErrorText(e));
      setBusy(false);
      handled.current = false;
    }
  }

  function onScan({ data }: BarcodeScanningResult) {
    if (handled.current) return;
    handled.current = true;
    const parsed = parsePairingPayload(data);
    if (!parsed.ok) {
      setError(parsed.error);
      setTimeout(() => (handled.current = false), 1500);
      return;
    }
    setScanning(false);
    void submit(parsed.url, parsed.code);
  }

  function submitManual() {
    const u = normalizeUrl(url);
    const c = normalizeCode(code);
    if (!u) return setError("Enter the relay's https:// address, e.g. https://hermes.lab.example.");
    if (!c) return setError("The code has 12 letters and digits, like ABCD-EFGH-JKMN.");
    void submit(u, c);
  }

  async function startScan() {
    setError(null);
    const granted = permission?.granted || (await requestPermission()).granted;
    if (granted) setScanning(true);
    else setError("Camera access was refused. Allow it in Android settings, or enter the code by hand.");
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: p.background }}>
      <KeyboardAvoidingView behavior="padding" className="flex-1">
        <ScrollView contentContainerClassName="flex-grow justify-center gap-6 p-6" keyboardShouldPersistTaps="handled">
          <View className="items-center gap-3">
            <Sigil size={112} />
            <Text className="font-display text-5xl text-foreground">Trismegistus</Text>
            <Text className="font-display-italic text-lg text-muted-foreground">thrice-great messenger of Hermes</Text>
          </View>

          <View className="h-px bg-border" />

          {scanning ? (
            <View className="gap-3">
              <View className="aspect-square overflow-hidden rounded-lg border border-primary/50">
                <CameraView style={{ flex: 1 }} facing="back" barcodeScannerSettings={{ barcodeTypes: ["qr"] }} onBarcodeScanned={onScan} />
              </View>
              <Text className="text-center text-sm text-muted-foreground">Point the camera at the QR code printed by `pair`.</Text>
              <Button variant="outline" onPress={() => setScanning(false)}>
                <Text>Cancel</Text>
              </Button>
            </View>
          ) : (
            <Card className="gap-3 p-5">
              <CardTitle>Pair with your relay</CardTitle>
              <CardDescription>
                On the server, run `trismegistus-relay pair --name &lt;device&gt;` and scan the code it prints. The camera
                is only used for that scan.
              </CardDescription>
              <Button onPress={startScan} disabled={busy}>
                <Text>Scan pairing QR</Text>
              </Button>
              {!manual ? (
                <Button variant="ghost" onPress={() => setManual(true)} disabled={busy}>
                  <Text className="text-muted-foreground">Enter the code by hand</Text>
                </Button>
              ) : (
                <View className="gap-3 pt-2">
                  <Input value={url} onChangeText={setUrl} autoCapitalize="none" autoCorrect={false} keyboardType="url" placeholder="https://relay.example" accessibilityLabel="Relay URL" />
                  <Input
                    value={code}
                    onChangeText={setCode}
                    autoCapitalize="characters"
                    autoCorrect={false}
                    placeholder="XXXX-XXXX-XXXX"
                    className="font-mono tracking-widest"
                    accessibilityLabel="Pairing code"
                    onSubmitEditing={submitManual}
                  />
                  <Button variant="secondary" onPress={submitManual} disabled={busy}>
                    <Text>Pair</Text>
                  </Button>
                </View>
              )}
            </Card>
          )}

          {busy && <ActivityIndicator color={p.primary} />}
          {error && (
            <View className="rounded-md border border-destructive/50 bg-destructive/10 p-3" accessibilityLiveRegion="polite">
              <Text className="text-sm text-destructive">{error}</Text>
            </View>
          )}
        </ScrollView>
      </KeyboardAvoidingView>
    </SafeAreaView>
  );
}
