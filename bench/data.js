window.BENCHMARK_DATA = {
  "lastUpdate": 1781599532693,
  "repoUrl": "https://github.com/stepan662/gent-go",
  "entries": {
    "gent throughput": [
      {
        "commit": {
          "author": {
            "email": "granat.stepan@gmail.com",
            "name": "Štěpán Granát",
            "username": "stepan662"
          },
          "committer": {
            "email": "granat.stepan@gmail.com",
            "name": "Štěpán Granát",
            "username": "stepan662"
          },
          "distinct": true,
          "id": "e996458917d3bfeceeadf6ddad0c0257843c0e7d",
          "message": "chore: better bench",
          "timestamp": "2026-06-15T23:28:32+02:00",
          "tree_id": "92b4566e1b3a36a0247ce0e257f8e52127c73aa4",
          "url": "https://github.com/stepan662/gent-go/commit/e996458917d3bfeceeadf6ddad0c0257843c0e7d"
        },
        "date": 1781559091025,
        "tool": "customBiggerIsBetter",
        "benches": [
          {
            "name": "spawn deep sqlite",
            "value": 92,
            "unit": "inst/s",
            "extra": "AMD EPYC 9V74 80-Core Processor · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          },
          {
            "name": "spawn deep postgres",
            "value": 311,
            "unit": "inst/s",
            "extra": "AMD EPYC 9V74 80-Core Processor · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          },
          {
            "name": "spawn recursive sqlite",
            "value": 94,
            "unit": "inst/s",
            "extra": "AMD EPYC 9V74 80-Core Processor · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          },
          {
            "name": "spawn recursive postgres",
            "value": 478,
            "unit": "inst/s",
            "extra": "AMD EPYC 9V74 80-Core Processor · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          }
        ]
      },
      {
        "commit": {
          "author": {
            "name": "Štěpán Granát",
            "username": "stepan662",
            "email": "granat.stepan@gmail.com"
          },
          "committer": {
            "name": "Štěpán Granát",
            "username": "stepan662",
            "email": "granat.stepan@gmail.com"
          },
          "id": "e996458917d3bfeceeadf6ddad0c0257843c0e7d",
          "message": "chore: better bench",
          "timestamp": "2026-06-15T21:28:32Z",
          "url": "https://github.com/stepan662/gent-go/commit/e996458917d3bfeceeadf6ddad0c0257843c0e7d"
        },
        "date": 1781599531907,
        "tool": "customBiggerIsBetter",
        "benches": [
          {
            "name": "spawn deep sqlite",
            "value": 138,
            "unit": "inst/s",
            "extra": "Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          },
          {
            "name": "spawn deep postgres",
            "value": 345,
            "unit": "inst/s",
            "extra": "Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          },
          {
            "name": "spawn recursive sqlite",
            "value": 134,
            "unit": "inst/s",
            "extra": "AMD EPYC 7763 64-Core Processor · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          },
          {
            "name": "spawn recursive postgres",
            "value": 486,
            "unit": "inst/s",
            "extra": "AMD EPYC 7763 64-Core Processor · 4 cores · 16GB · linux x64 6.17.0-1018-azure"
          }
        ]
      }
    ]
  }
}