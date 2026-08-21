---
title: Principles
description: The six invariants of the system. Every implementation decision answers to them.
sidebar:
  order: 2
---

## P1: Discovery and verification are two questions, not two tools

The same engine answers both, with a different input and a different mandate.

| | Discovery | Verification |
|---|---|---|
| Input | a domain, plus the [scope rules](/architecture/scope/) | the hosts already in the inventory |
| Question | what exists under this perimeter | what still answers |
| Trigger | a program's cadence, or a scope change | an asset's due date |
| Authoritative on | presence | absence |
| Recursion | yes, sources feed sources | no, the target set is fixed |
| Cost driver | the sources and the size of what they return | the number of due assets |

One binary, one report shape, one ingestion path. What differs is what each run is allowed to
conclude, which is the subject of P2.

## P2: Absence of proof is not proof of absence

**Discovery is authoritative on presence and never on absence.** A source that rate-limits, an index
that moved, a key that expired: the host drops out of the report while nothing changed on the
target. FastRecon records every source with its status precisely so that this is visible, and
visibility is all it buys. Recon can refuse to conclude. It cannot conclude.

Only a run over an **explicit target list** can move an asset toward death, because only there does
a missing answer mean something. That is why [verification](/architecture/verification/) feeds the
scanner the inventory rather than the output of enumeration.

:::note
Comparing two enumeration runs to declare assets gone is the classic home-grown ASM bug. It produces
dozens of false positives a day and teaches its owner to ignore the alerts.
:::

## P3: Nothing is ever deleted

An asset is never `DELETE`d. It ages, changes state, and ends up archived. An asset out of scope
today can be in scope in six months, and its history is what makes that useful.

## P4: Control belongs to the control plane

Scanners are **replaceable capabilities behind a contract**: the input is a run definition, the
output is a report. Scope, scheduling, correlation, diff and authorization live in the control
plane. That is where the value sits; the tools that send packets are interchangeable, and two of
them already are.

## P5: No arbitrary code execution in configuration

A run is parameterized by **data**: port lists, a stage scope, exclusion patterns, a source
selection. Never by commands. A design where defining a workflow means writing shell is structurally
incompatible with letting anyone else use the platform, since editing a workflow would be remote
code execution by design.

## P6: Scanners are untrusted by assumption

A run receives a frozen perimeter and returns a report. It holds no database credential and no
visibility onto other tenants. Everything a scanner needs travels in its run definition, and
everything it claims is re-derived on arrival.
