# Corpova Consensus Engine — Phase 1–5
**Session: 2026-07-23**

## Mi volt a kiindulás

A motor a "vaccines cause autism" állításra `evidence-supports`,
score `+0.18`, 15 supporting művet adott. Vagyis a tudományos
konszenzus ELLENKEZŐJÉT állította.

## Mi lett a végeredmény

`evidence-refutes`, score `−1.00`, confidence `0.74`,
0 supporting / 6 refuting / 0 mixed / 5 inconclusive,
11 releváns mű a 15-ből.

---

## Commit-lánc

### Monorepo — `myfork/scientific-consensus-pr`
| Hash | Tartalom |
|---|---|
| `0d3e2b3b4` | Phase 1 — low-evidence guard + near-unanimous |
| `91e55e04d` | Phase 2 + Option C — stance classifier harden |
| `7c1128228` | Phase 3 — PICO relevance gate |
| `2230844bf` | Phase 4 — confidence dispersion penalty |
| `e64694841` | Phase 5 — apex fix + evidence tier ordering |

### pubvera-corpova — `origin/main`
| Hash | Tartalom |
|---|---|
| `20a3bd7` | Phase 1 UI + Phase 2/C binary |
| `137597f` | angol szövegek a sávokban |
| `86f39b7` | vendor: Phase 3 |
| `4d5ed5f` | vendor: Phase 4 |
| `df553a8` | vendor: Phase 5 |

GitHub Actions: mind a 6 run zöld.

---

## Mit csinál most a motor, fázisonként

### Phase 1 — Low-evidence guard
`ApplyLowEvidenceGuard`: ha nem-LLM útról jön ÉS `relevantCount < 5`,
az evidence strength `insufficient` lesz.
`nearUnanimous(r)`: `|score| >= 0.98` ÉS `refuting == 0` ÉS `mixed == 0`
→ figyelmeztetés, hogy talán kiszűrődött a valódi vita.
Új JSON mezők: `relevant_count`, `near_unanimous`, `evidence_guarded`.

**Ismert korlát:** a `nearUnanimous` első sora kizárja a `refuting > 0`
esetet, ezért gyakorlatilag csak pozitív score-nál tud tüzelni.
"Egyhangú cáfolat is gyanús" védelemhez KÜLÖN ág kellene.

### Phase 2 — Stance classifier (stance.go)
- **A:** claim-pair gate — ~120 karakteres, mondathatárral vágott
  ablak, benne intervention ÉS outcome token
- **B1:** negáció-kapu, 25 karakteres visszatekintés
- **B2:** framing-kapu (`Objective:`, `whether`, `hypothes*`,
  `concern`, `belief`, `myth`, `misinformation`)
- pozíció-alapú dedup
- **B3** (clause-negáció) MÉRÉSSEL ELVETVE: 1 mű haszon, fals-negatív
  kockázat. A tesztje elfogadott korláttá alakítva.

### Option C — Refutation cues (stance.go, csak harm-ág)
- `strongRefutCues` (6 regex): "did/does not cause", "does not support
  a causal", "no causal link/association", "lack of association",
  "not associated with increased risk" — full text, B2 érvényes,
  nincs pairing
- `metaRefutCues` (6 regex): "evidence against", refut*, debunk*,
  retract*, fraud* — CSAK `metaRefutPairing()`-gel (±80 karakter,
  intervention ÉS outcome kell)
- `optCEnabled` kapcsoló (alapérték true)

**Mérés (39 mű):** vaccines refuting 13→16, inconclusive 25→22.
sweetener korpusz 0 változás. Mind a 3 "MUST BE 0" regresszió = 0.

**Flippelt művek:**
- #22 "Vaccines Did Not Cause Rachel's Autism" → strongRefutCues[0]
- #31 "Evidence Does Not Support a Causal Association" → strongRefutCues[1]
- #05 "The Vaccine-Autism Connection..." → metaRefutCues[5] "fraudulent"
  + pairing. A cím TÁMOGATÓNAK hangzik, valójában a Wakefield-csalásról
  szól. A pairing-kapu bizonyította, hogy a mi állításunkról van szó.

### Phase 3 — PICO relevance gate (pico.go)
`IsPICORelevant(abstract, title, ivTokens, outTokens)`: az intervention
ÉS az outcome is szerepeljen az abstractben vagy a címben.
5 karakteres stemek, kisbetűsítés, stopszó-szűrés.

**Kritikus javítás (v1 → v2):** a `PICOTokens()` eleinte oldalanként
csak az ELSŐ tokent vette. Az "artificial sweeteners" esetében ez az
"artif" stem, holott a lényegi fej a "sweeteners". A szakirodalom
"low-calorie sweeteners", "non-nutritive sweeteners" formában ír róla.
Javítás: oldalanként BÁRMELYIK token elég (OR az oldalon belül),
a két oldal között marad az ÉS.

**Mérés:**
| | v1 (első token) | v2 (OR) |
|---|---|---|
| vaccines drop | 28% | 28% |
| sweetener drop | 69% | 46% |
| sweetener confidence | 0.65 | 0.86 |
| sweetener strength | moderate | high |

9 mű visszanyerve az OR-fixszel, köztük 2 meta-analízis pontosan
a témáról.

**Kidobott művek ellenőrizve:** 10-ből 9 egyértelműen helyes
(cardiovascular, microbiota, inflammation, antibiotic — mind más
kimenetről szól).

### Phase 4 — Confidence dispersion (score.go)
Két csendes hiba javítva:

**1. Láthatatlan bizonytalanság.** Az `agreement` tag csak
`(supporting + refuting)`-gal osztott, tehát a mixed és az
inconclusive TELJESEN láthatatlan volt. Egy 1 supporting /
30 inconclusive korpusz tökéletes 1.0 egyetértést mutatott.

**2. Fordított rangsor.** 100 egyenletesen megosztott mű magasabb
confidence-t kapott (0.67), mint 10 egyetértő (0.65).

```go
func stanceDispersion(s, r, m, i int) float64 {
    total := s + r + m + i
    if total == 0 { return 0 }
    net := abs(s - r)
    return 1 - float64(net)/float64(total)
}
conf = conf * (1 - dispersionWeight*dispersion)
```

`var dispersionWeight = 0.35` (var, nem const — jövőbeli kalibrációhoz)
`var phase4ConfidenceEnabled = true` (escape hatch)

**Kalibráció (súly-sweep):**
| korpusz | dispersion | OFF | 0.20 | 0.25 | 0.35 | 0.50 |
|---|---|---|---|---|---|---|
| vaccines | 0.536 | 0.91 | 0.81 | 0.79 | **0.74** | 0.67 |
| sweeteners | 0.524 | 0.86 | 0.77 | 0.75 | **0.70** | 0.63 |

Verdict/score/strength MIND A NÉGY súlynál változatlan.
Választott súly: **0.35**.

Emellett: a `compare.go` is megkapta a PICO-kaput, így a consensus,
a compare és a batch ugyanazt a részhalmazt méri.

### Phase 5 — Apex + megjelenítési rendezés
**Fix A:** üres releváns halmaznál az `apex_design` üres sztring volt
(`""`), mert a `Consensus()` korán visszatér, mielőtt az `ApexDesign()`
lefutna. A Phase 3 PICO-kapu tette ezt az ágat gyakorivá.
Javítva mindkét helyen → `"unclassified"`.

**Fix B:** a `topByStance` korábban CSAK `CitedBy` szerint rendezett,
ami rendszeresen a régi művet hozza előre (egy 1998-as kohorsznak több
idézete van, mint egy 2024-es meta-analízisnek). Új rendezés:
elsődleges kulcs `TierRank` növekvő, másodlagos `CitedBy` csökkenő.

⚠️ Ez a lista TAGSÁGÁT is változtatja, nem csak a sorrendet, mert a
vágás a rendezés UTÁN történik. Szándékos, teszt dokumentálja
(`TestTopByStanceCutChangesMembership`).

**Mérés (top_refuting, before → after):**
- vaccines: narrative-review (rank 9, 1211 idézet) kiesett,
  helyette cohort-study (rank 4, 245 idézet)
- sweeteners: RCT (rank 3, 818 idézet) kiesett,
  helyette meta-analysis (rank 1, 272 idézet)
- 1 mű cserélődött korpuszonként, minden invariáns tartott

`var phase5SortEnabled = true` (kikapcsolva pontosan a régi sorrend)

---

## Fájlhelyek

### Motor — monorepo (`~/Desktop/printing-press-library`, branch `scientific-consensus-pr`)
Gyökér: `library/other/scientific-consensus/`

| Fájl | Fázis | Tartalom |
|---|---|---|
| `internal/scengine/stance.go` | 2 + C | stance-osztályozó, claim-pair/negáció/framing kapuk, refutation cues |
| `internal/scengine/pico.go` | 3 | `IsPICORelevant`, `PICOTokens`, `picoGateEnabled` |
| `internal/scengine/score.go` | 1 + 4 + 5 | `Consensus`, `confidence`, `stanceDispersion`, `ApplyLowEvidenceGuard`, `nearUnanimous`, apex seed |
| `internal/scengine/evidence.go` | 5 | `ApexDesign`, `TierRank`, `PyramidOrder` |
| `internal/cli/consensus.go` | 3 + 5 | PICO-kapu bekötés, `topByStance` tier-first rendezés, `phase5SortEnabled` |
| `internal/cli/compare.go` | 4 | PICO-kapu bekötés (`compare` + `batch`) |
| `internal/cli/consensus_sort_test.go` | 5 | kézi rendezési tesztek (a generált `consensus_test.go` mellett) |
| `internal/scengine/scengine_test.go` | 1–5 | fő teszttábla: dispersion, confidence baseline, apex, PICO |

### Kapcsolók (mind `var … = true`, escape hatch + toggle-off regressziós teszt)
- `picoGateEnabled` — `pico.go`
- `phase4ConfidenceEnabled` — `score.go`
- `phase5SortEnabled` — `cli/consensus.go`
- `optCEnabled` — `stance.go`
- `var dispersionWeight = 0.35` — `score.go` (var, nem const)

### Reprint-guard bejegyzések
`library/other/scientific-consensus/.printing-press-patches/`
- `claim-aware-stance-polarity-and-claim-relevance-gate.json` (Phase 2)
- `harm-context-increase-cues-and-deterministic-gap-order.json` (Option C)
- `pico-relevance-gate-requires-both-claim-sides.json` (Phase 3)
- `confidence-penalizes-a-divided-corpus.json` (Phase 4)
- `evidence-tier-orders-the-top-cards.json` (Phase 5)

### Web app (`~/pubvera-corpova`, `origin/main`)
- `main.go` — HTTP wrapper, keyless child, BYOK LLM-szintézis, divergence flag
- `index.html` — UI, 3 tanácsadó sáv (angol), dampening
- `bin/scientific-consensus-pp-cli.exe` — vendorozott CLI bináris
- `bin/server.exe` — vendorozott szerver bináris

### Mérési harness (ideiglenes, minden mérés után törölve)
Befagyasztott korpuszok:
`…/scratchpad/corpus_vax.json` (vaccines→autism, N=40)
`…/scratchpad/corpus_sweet.json` (sweeteners→weight gain, N=40)
A harness a valódi pipeline-t futtatja
(`filterRelevant` → PICO → `scoreWorks` → `Consensus`), `SCDIAG=1` és
`SCDIAG_CORPUS_DIR` környezeti változókkal.
