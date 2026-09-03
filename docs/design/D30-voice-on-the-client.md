# D30: voice is an interface, and it is rendered on the client

**Inputs**, from Nate on 2026-09-02, late: record Kokoro in the design docs
as the way to interface with the platform by voice, with the rendering done
**client side**. Same night, the fleet gained a server-side voice stack
(brh-infra `stacks/voice`: Kokoro-FastAPI behind the OpenAI speech API) so
the Claude Code profiles on the workstation could speak. This entry records
how the two relate and why the platform's own clients do not use the server.

## Decision: text goes down the wire, speech is made where it is heard

- **The browser client and the desktop client synthesise speech locally**
  from the text the daemon already streams to them. Kokoro (82M parameters)
  runs in the client: ONNX Runtime with WebGPU where the browser has it, WASM
  where it does not; the desktop client links the same model natively. The
  model is fetched once and cached by the client.
- **The daemon never produces audio.** It has no voice endpoint and no audio
  in the event log. What a person heard is exactly what they were shown, so
  captions are the transcript and provenance (invariant 9) has nothing new
  to track.
- **The voice is part of the agent's definition** (D27): a voice id on the
  agent record, honoured by every client, so an agent sounds the same in the
  browser, on the desktop, and from the workstation hook. Definition, not
  credential; changing it is a normal edit with a normal diff.
- **Speech starts at the first sentence boundary** of the streamed text, not
  at the end of the turn. The client chunks on sentence punctuation and
  queues; a new turn from the same agent cancels the queue.
- **The renderer is swappable behind the OpenAI speech request shape**
  inside the client: the same `{input, voice, speed}` the server stack
  accepts, so a client that cannot run a model (or a user who opts out) can
  point that call at a server instead, and nothing above the call changes.

## Why the client

1. **The text is already there.** Sending it back up to be turned into audio
   and streamed down again adds a round trip, a server cost, and a second
   copy of the conversation in flight, for nothing the server needs.
2. **It scales with the people using it**, not with the host. The fleet host
   is a desktop with an AMD GPU and no ROCm build of the engine; per-client
   CPU synthesis on that box would be the first thing to fall over.
3. **It works offline** once the model is cached, which matters for the
   desktop client on a laptop.
4. **Latency.** Local synthesis on a sentence is faster than real time on
   any recent machine; a server hop is not.

## Where the server stack fits

`stacks/voice` exists for clients that cannot run a model: the Claude Code
Stop hook on the workstation (a shell script), Home Assistant, anything on
the LAN that speaks the OpenAI API. It is the fallback the client's swappable
call points at, and it is where a future engine is trialled fleet-wide before
the client bundle changes. It is not the platform's voice.

## What lost

- *Server-rendered audio streamed to clients*: every reason above.
- *The browser's built-in `speechSynthesis`*: voices differ per OS and
  browser, several are poor, and an agent's voice identity cannot survive it.
- *A voice per client instead of per agent*: the agent is the thing with an
  identity; the client is a window onto it.

## Left open, deliberately

- **Input.** Speech-to-text was not decided tonight. Client-side Whisper is
  the symmetric choice and has the same reasons going for it; it is not
  decided, and push-to-talk versus wake word even less so.
- **Model distribution.** Whether the client fetches the ONNX files from the
  public model hub or the instance serves them (which keeps clients off a
  third-party CDN and would belong in the instance repository under D25).
  Leaning to the instance serving them; not decided.
- **Interruption.** Whether a person speaking (once input exists) cuts the
  agent's speech, and whether that cut is recorded as an event.
- **Familiars.** The waggle familiars are tool-locked and journaled on their
  behalf (D26); whether they get a voice, and whose client renders it, waits
  on the first person to ask.
