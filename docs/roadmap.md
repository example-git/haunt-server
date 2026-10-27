# Project direction

Haunt implements independent community services and a shared browser/desktop
client. Current features are described in the component READMEs and API docs.
The goal is convenient messaging and calls for small communities with predictable
storage, bandwidth and operational costs. Accounts remain scoped to their servers.

Ongoing work includes wider device/browser coverage, release and recovery testing,
and independent review of the DM encryption implementation. The shared client core
allows future Android messaging without requiring mobile voice in the first release.
Federated accounts and a public centralized hosting platform are not current goals.

A later extension category will support server-owned LLM chatbots. Each server will
own bot identities, allowed channels, configuration, conversation context and jobs.
Mention/reply triggers and dedicated bot channels will use the existing extension
administration model with a versioned chat protocol. Inference jobs will run
separately from message delivery and history reads, with durable deduplication.
Model endpoints may be local or external; identities and memory remain scoped to
one server. Utility bots and federated bot identities are outside this stage.

See [testing](testing.md) for the verification harnesses and their limits.
