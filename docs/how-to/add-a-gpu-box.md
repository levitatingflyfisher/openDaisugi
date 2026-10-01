# Add a GPU box to the floor

A second machine with a GPU can serve models for the whole floor. You do four
things on it, one profile on this box, and look at the Machines panel to see
it work. The panel only reads. It never starts, stops or edits anything on
any box.

## 1. Set up the box

Install Omarchy, or any Linux you like. Install Tailscale on it and join the
same tailnet as this box:

```sh
sudo tailscale up
```

Tailscale's console is where a machine is added and where the access rules
live. Coppice never changes either.

## 2. Run a model server on it

Run llama.cpp or vLLM, and bind it to the tailnet, not only to loopback. An
OpenAI-style server is the plainest choice:

```sh
llama-server -m ~/models/your-model.gguf --host 0.0.0.0 --port 8000
```

On a box that other people can reach, bind the server to the tailnet
address (`tailscale ip -4`) and keep the Tailscale access rules tight.

## 3. Add a profile on this box

In `~/.opendaisugi/config.yaml`, give the server a name. The `base_url` host
is the box's MagicDNS name (the name in the Tailscale console) or its tailnet
address:

```yaml
agent_profiles:
  gpu:
    wire: openai
    base_url: http://gpu.tail1234.ts.net:8000/v1
    model: your-model
```

See [agent profiles](agent-profiles.md) for the fields, and
[run the foreman on any model](foreman-any-model.md) to put the foreman there.

## 4. Look at Machines

Open the web floor and press Machines beside Settings (on a phone it rises as
a sheet). Each box on the tailnet has a row: an online dot, its name and
system, and under it each profile that points at it with the time a
connection takes, for example `gpu  gpu.tail1234.ts.net:8000  12 ms`. A box
that no profile points at says `No profile points here.`

A profile whose key variable is not set says `Key not set.`; export the
variable the profile names, then open Machines again.

If a profile says `No answer`, the box is up but the server is not: check
that the model server runs and listens on the tailnet address. If the box is
offline, its row says when it was last seen.

If Tailscale is not on this box or is not running, the panel says so in one
line, with the command to run.

The same facts are the `floor.machines` verb (see the coppice PROTOCOL, The
machines view). The check is one TCP connection to each profile's host and
port. It sends no request to the model server and never sends a key.
