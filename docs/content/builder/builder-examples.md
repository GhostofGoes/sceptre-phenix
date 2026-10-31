# Builder Examples

The examples in the builder docs primarily use one small lab, **Riverside Water**. It is a
water utility with an internet edge, a DMZ, a corporate network, and an OT
(control) network behind a firewall. It has two topologies, `riverside-water` and `corp-services`, and a scenario, `riverside-water`.

Some pages use two additional topologies: `metro-campus` for the layouts, and `pump-station`.

## Load the example configs

### Download the example files

- [corp-services.topology.yaml](examples/corp-services.topology.yaml)
- [riverside-water.topology.yaml](examples/riverside-water.topology.yaml)
- [riverside-water.scenario.yaml](examples/riverside-water.scenario.yaml)
- [metro-campus.topology.yaml](examples/metro-campus.topology.yaml)
- [pump-station.topology.yaml](examples/pump-station.topology.yaml)
- [pump-station.builder.json](examples/pump-station.builder.json)
- [riverside-water.builder.json](examples/riverside-water.builder.json) (see
  [The drafts on these pages](#the-drafts-on-these-pages))
- [lakeside-treatment.builder.json](examples/lakeside-treatment.builder.json),
  with layers (see
  [Example: map layers to the Purdue model](diagrams.md#example-map-layers-to-the-purdue-model))

Store the first four configs in phenix, and create the experiment
`riverside` from them.

### Loading from the CLI

```bash
phenix config create corp-services.topology.yaml riverside-water.topology.yaml riverside-water.scenario.yaml metro-campus.topology.yaml
phenix experiment create riverside -t riverside-water -s riverside-water
```

### Loading from the Web-UI

1. On the **Configs** page, upload the four configs (see
   [Create a Config](../configuration.md#create-a-config)).
2. On the **Experiments** page, create the experiment `riverside` with
   **Experiment Topology** `riverside-water` and **Experiment Scenario**
   `riverside-water` (see
   [Create a New Experiment](../experiments.md#create-a-new-experiment)).

## The drafts on these pages

Most pages show these drafts:

- **Riverside Water**: the import of `riverside-water`, renamed, laid out with
  **ELK layered**, grouped with **Auto-group** > **By network**, with a note
  and the scenario `riverside-water`. To get it,
  [upload](import-upload-download.md#uploading-a-builder-document)
  `riverside-water.builder.json` on the drafts page.
- **Riverside Water expansion**: a copy of Riverside Water with a duplicate
  of `historian-01`, which causes a warning. To make it:
    1. On the drafts page, upload `riverside-water.builder.json` again.
       Upload always makes a new draft, and the new draft opens.
    2. Use [**Edit diagram name**](editor.md#renaming-the-diagram) to rename
       the draft `Riverside Water expansion`.
    3. Select `historian-01`.
    4. Press <kbd>⌘</kbd>+<kbd>D</kbd> on macOS or
       <kbd>Ctrl</kbd>+<kbd>D</kbd> on Windows and Linux to
       [duplicate it](diagrams.md#duplicating-a-device). The copy is
       `historian-01-2`, and the header says **1 warning**.
- **Metro Campus**: the import of `metro-campus`, renamed Metro Campus, with
  no layout.

The [quick start](index.md#quick-start) and
[Publishing a topology](publishing.md#publishing-a-topology) each publish
`riverside-water`. After that, the other drafts made from `riverside-water`
cannot publish. Drafts that you then upload from
`riverside-water.builder.json` cannot publish either (see
[When the source config changed](publishing.md#when-the-source-config-changed)).
