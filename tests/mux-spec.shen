\\ Bifrost/Shen transition traces for the pure control-plane reducer.
(tc -)
(load "specs/mux.shen")

(define mux.assert
  {A --> A --> string --> boolean}
  X X _ -> true
  X Y Label -> (error (cn Label (cn ": expected equality, got "
                                  (cn (mux.show X) (cn " / " (mux.show Y)))))))

\\ str refuses lists, so a failing assertion on a transition result used to
\\ report "can't str pair object" instead of the label that failed.
(define mux.show
  {A --> string}
  X -> (trap-error (str X) (/. Err "<list>")))

(define mux.accepted?
  {(list A) --> boolean}
  ["accepted" _ _] -> true
  _ -> false)

(define mux.rejected?
  {(list A) --> string --> boolean}
  ["rejected" Reason] Reason -> true
  _ _ -> false)

\\ Every trace reduces against an explicit clock. 8000ms is the lease; the
\\ instants below are chosen so that 1000 is inside it and 9001 is past it.
(define mux.clk
  {number --> clock}
  Now -> [Now 8000])

(define mux.state
  {(list A) --> session}
  Result -> (head (tail Result)))

(define mux.effects
  {(list A) --> (list A)}
  Result -> (head (tail (tail Result))))

(define mux.trace-attach
  {number --> boolean}
  _ ->
    (let Dim [80 24]
      (let Snap [0 Dim 0 0 false ""]
        (let S0 [[] 0 Snap Dim false false [[] []] 0]
          (let Begin (mux.reduce S0 (mux.clk 1000) ["begin-attach" "client-1"])
            (let Finish (mux.reduce (mux.state Begin) (mux.clk 1000)
                                    ["finish-attach" "client-1" Snap])
              (let S1 (mux.state Finish)
                (and (mux.assert (mux.accepted? Begin) true "begin attach")
                     (and (mux.assert (mux.accepted? Finish) true "finish attach")
                          (and (mux.assert (mux.member? "client-1" (head S1))
                                             true "client attached")
                               (and (mux.assert (head (tail (tail (tail (tail S1)))))
                                                false "lock released")
                                    \\ Attaching is not exercising control.
                                    (mux.assert (mux.session-heard S1) 0
                                                "attach leaves no control evidence"))))))))))))

(define mux.base
  {number --> session}
  _ -> [["client-1"] 0 [0 [80 24] 0 0 false ""] [80 24] false false [[] []] 0])

(define mux.after-acquire
  {number --> session}
  N -> (mux.state (mux.reduce (mux.base N) (mux.clk 1000) ["acquire-control" "client-1"])))

(define mux.after-resize
  {session --> session}
  S -> (mux.state (mux.reduce S (mux.clk 1000) ["resize" "client-1" [100 40]])))

(define mux.after-output
  {session --> session}
  S -> (mux.state (mux.reduce S (mux.clk 1000) ["pty-output" 12])))

(define mux.after-exit
  {session --> session}
  S -> (mux.state (mux.reduce S (mux.clk 1000) ["process-exit" 7])))

(define mux.trace-control-and-events
  {number --> boolean}
  N ->
    (let S1 (mux.after-acquire N)
      (let S2 (mux.after-resize S1)
        (let S3 (mux.after-output S2)
          (let S4 (mux.after-exit S3)
            (and (mux.assert (head (tail S1)) 1 "control sequence")
                 (and (mux.assert (head (tail S2)) 2 "resize sequence")
                      (and (mux.assert (head (tail S3)) 3 "output sequence")
                           (mux.assert (head (tail S4)) 4 "exit sequence")))))))))

(define mux.trace-rejections
  {number --> boolean}
  _ ->
    (let S [["owner" "observer"] 0 [0 [80 24] 0 0 false ""] [80 24]
            false false [["owner"] []] 1000]
      (and (mux.assert (mux.rejected? (mux.reduce S (mux.clk 1000) ["input" "observer" 1])
                                      "control-owned") true "observer rejected")
           (and (mux.assert (mux.rejected? (mux.reduce S (mux.clk 1000) ["input" "missing" 1])
                                           "not-attached") true "missing rejected")
                (mux.assert (mux.rejected? (mux.reduce S (mux.clk 1000) ["attach" "owner"])
                                        "already-attached") true "duplicate rejected")))))

(define mux.trace-barriers
  {number --> boolean}
  _ ->
    (let Dim [80 24]
      (let Snap [0 Dim 0 0 false ""]
        (let S0 [[] 0 Snap Dim false false [[] []] 0]
          (let Begin (mux.reduce S0 (mux.clk 1000) ["begin-attach" "client-1"])
            (let Locked (mux.state Begin)
              (and
                (mux.assert (mux.rejected? (mux.reduce Locked (mux.clk 1000)
                                                       ["finish-attach" "client-2" Snap])
                                            "snapshot-owner-mismatch") true "attach owner mismatch")
                (and
                  (mux.assert (mux.rejected? (mux.reduce Locked (mux.clk 1000)
                                                         ["finish-attach" "client-1"
                                                          [1 Dim 0 0 false ""]])
                                              "snapshot-mismatch") true "attach sequence mismatch")
                  (and
                    (mux.assert (mux.rejected? (mux.reduce Locked (mux.clk 1000)
                                                           ["begin-attach" "client-2"])
                                                "writer-locked") true "attach lock exclusion")
                    (mux.assert (mux.accepted? (mux.reduce Locked (mux.clk 1000)
                                                           ["finish-attach" "client-1" Snap]))
                                true "attach completion"))))))))))

(define mux.trace-command-contract
  {number --> boolean}
  N ->
    (let Base (mux.base N)
      (let Acquired (mux.reduce Base (mux.clk 1000) ["acquire-control" "client-1"])
        (let Controlled (mux.state Acquired)
          (let Input (mux.reduce Controlled (mux.clk 1000) ["input" "client-1" 41])
            (let Resize (mux.reduce Controlled (mux.clk 1000) ["resize" "client-1" [100 40]])
              (let Released (mux.reduce Controlled (mux.clk 1000) ["release-control" "client-1"])
                (let Leased (mux.reduce Controlled (mux.clk 1000) ["lease-expired" "client-1"])
                  (let Detached (mux.reduce Controlled (mux.clk 1000) ["detach" "client-1"])
                    (let Exited (mux.reduce Controlled (mux.clk 1000) ["process-exit" 7])
                      (let ExitedState (mux.state Exited)
                        (and
                          (mux.assert (mux.effects Input)
                                      [["write-pty" "client-1" 41]] "input effect")
                          (and
                            (mux.assert (mux.effects Resize)
                                        [["resize-pty" [100 40]] ["publish" 2 "delta" 0]]
                                        "resize effects")
                            (and
                              (mux.assert (head (tail (mux.state Released))) 2 "release sequence")
                              (and
                                (mux.assert (head (tail (mux.state Leased))) 2 "lease sequence")
                                (and
                                  (mux.assert (mux.member? "client-1" (head (mux.state Detached)))
                                              false "detach membership")
                                  (and
                                    (mux.assert (mux.rejected? (mux.reduce Base (mux.clk 1000)
                                                                           ["resync" "missing"])
                                                               "not-attached") true "resync membership")
                                    (and
                                      (mux.assert (mux.accepted? (mux.reduce Base (mux.clk 1000)
                                                                             ["resync" "client-1"]))
                                                  true "resync attached")
                                      (and
                                        (mux.assert (mux.rejected? (mux.reduce ExitedState (mux.clk 1000)
                                                                               ["pty-output" 9])
                                                                   "exited") true "output after exit")
                                        (and
                                          (mux.assert (mux.rejected? (mux.reduce ExitedState (mux.clk 1000)
                                                                                 ["process-exit" 8])
                                                                     "exited") true "repeat exit")
                                          (mux.assert (mux.rejected? (mux.reduce Base (mux.clk 1000)
                                                                                 ["bogus"])
                                                                     "unknown-command") true
                                                      "unknown command")))))))))))))))))))))

\\ -------------------------------------------------------------------------
\\ Ownership is an assertion with an age
\\
\\ Two clients attached. client-1 takes control at 1000 and then does nothing.
\\ Bug: a control lease parked on a client nobody can reach made the session
\\ untypeable for everyone else, permanently.

(define mux.two-clients
  {number --> session}
  _ -> [["client-2" "client-1"] 0 [0 [80 24] 0 0 false ""] [80 24] false false [[] []] 0])

(define mux.owned-at-1000
  {number --> session}
  N -> (mux.state (mux.reduce (mux.two-clients N) (mux.clk 1000)
                              ["acquire-control" "client-1"])))

(define mux.trace-lease-preemption
  {number --> boolean}
  N ->
    (let Owned (mux.owned-at-1000 N)
      (let Fresh (mux.reduce Owned (mux.clk 2000) ["acquire-control" "client-2"])
        (let Edge (mux.reduce Owned (mux.clk 9000) ["acquire-control" "client-2"])
          (let Stale (mux.reduce Owned (mux.clk 9001) ["acquire-control" "client-2"])
    (and (mux.assert (mux.session-heard Owned) 1000 "acquisition is evidence")
      (and (mux.assert (mux.rejected? Fresh "control-owned") true "a fresh owner still excludes")
        (and (mux.assert (mux.rejected? Edge "control-owned") true "the last instant of the lease still excludes")
          (and (mux.assert (mux.accepted? Stale) true "a stale owner does not exclude")
            (and (mux.assert (mux.has-control? (mux.state Stale) "client-2") true "the competing claim takes control")
              (mux.assert (mux.session-heard (mux.state Stale)) 9001 "the new owner starts its own lease")))))))))))

\\ Preemption is not eviction. A quiet owner that types after the lease window
\\ is still the owner and re-proves its claim; nothing kicks it out.
(define mux.trace-quiet-owner-is-not-evicted
  {number --> boolean}
  N ->
    (let Owned (mux.owned-at-1000 N)
      (let Late (mux.reduce Owned (mux.clk 50000) ["input" "client-1" 7])
        (let Resized (mux.reduce Owned (mux.clk 50000) ["resize" "client-1" [100 40]])
    (and (mux.assert (mux.accepted? Late) true "a quiet owner may still type")
      (and (mux.assert (mux.session-heard (mux.state Late)) 50000 "typing re-proves the claim")
        (and (mux.assert (mux.accepted? Resized) true "a quiet owner may still resize")
          (mux.assert (mux.session-heard (mux.state Resized)) 50000 "resizing re-proves the claim"))))))))

\\ The other half of the rule, and the one that matters: only exercising control
\\ counts. A keepalive proves a process is running, and the process holding a
\\ lease is routinely a relay proxy for a browser that has closed. Commands that
\\ are not control must leave the evidence exactly where it was.
(define mux.trace-only-control-renews
  {number --> boolean}
  N ->
    (let Owned (mux.owned-at-1000 N)
      (let Resynced (mux.reduce Owned (mux.clk 50000) ["resync" "client-1"])
        (let Output (mux.reduce Owned (mux.clk 50000) ["pty-output" 3])
          (let Joined (mux.reduce Owned (mux.clk 50000) ["attach" "client-3"])
    (and (mux.assert (mux.session-heard (mux.state Resynced)) 1000 "resync is not evidence of control")
      (and (mux.assert (mux.session-heard (mux.state Output)) 1000 "pty output is not evidence of control")
        (mux.assert (mux.session-heard (mux.state Joined)) 1000 "another client attaching is not evidence of control"))))))))

\\ Releasing ownership must take the evidence with it, or the next acquisition
\\ would inherit a stranger's freshness.
(define mux.trace-release-clears-evidence
  {number --> boolean}
  N ->
    (let Owned (mux.owned-at-1000 N)
      (let Released (mux.reduce Owned (mux.clk 2000) ["release-control" "client-1"])
        (let Detached (mux.reduce Owned (mux.clk 2000) ["detach" "client-1"])
          (let Expired (mux.reduce Owned (mux.clk 2000) ["lease-expired" "client-1"])
            (let Bystander (mux.reduce Owned (mux.clk 2000) ["detach" "client-2"])
    (and (mux.assert (mux.session-heard (mux.state Released)) 0 "release clears evidence")
      (and (mux.assert (mux.session-heard (mux.state Detached)) 0 "detach clears evidence")
        (and (mux.assert (mux.session-heard (mux.state Expired)) 0 "lease expiry clears evidence")
          (mux.assert (mux.session-heard (mux.state Bystander)) 1000 "a bystander leaving is not the owner's business"))))))))))

\\ -------------------------------------------------------------------------
\\ Delivery
\\
\\ Bug: a reply that could not be routed returned from the daemon run loop and
\\ took the shell and scrollback with it. Twice: once for the routing errors,
\\ once for an encode failure nobody had thought of. The arm that matters is
\\ the default one.
(define mux.trace-delivery-classification
  {number --> boolean}
  _ ->
    (and (mux.assert (mux.delivery-fatal? "socket-closed") true "a dead socket is the session's problem")
      (and (mux.assert (mux.delivery-fatal? "no-route") false "no route is one peer")
        (and (mux.assert (mux.delivery-fatal? "no-identity") false "no identity is one peer")
          (and (mux.assert (mux.delivery-fatal? "not-draining") false "a peer that stopped reading is one peer")
            (and (mux.assert (mux.delivery-fatal? "encode-failed") false "an answer we could not build is one answer")
              (and (mux.assert (mux.delivery-fatal? "unknown") false "an unrecognised outcome is one peer")
                (mux.assert (mux.delivery-fatal? "some-outcome-invented-later") false "so is one nobody has named yet"))))))))

\\ Teardown after a delivery failure is idempotent, so no failure path has to
\\ work out how far the client got.
(define mux.trace-peer-lost-is-idempotent
  {number --> boolean}
  N ->
    (let Owned (mux.owned-at-1000 N)
      (let Lost (mux.reduce Owned (mux.clk 2000) ["peer-lost" "client-1"])
        (let Again (mux.reduce (mux.state Lost) (mux.clk 2000) ["peer-lost" "client-1"])
          (let Stranger (mux.reduce Owned (mux.clk 2000) ["peer-lost" "never-attached"])
    (and (mux.assert (mux.accepted? Lost) true "losing a peer is accepted")
      (and (mux.assert (mux.member? "client-1" (head (mux.state Lost))) false "the lost peer is detached")
        (and (mux.assert (mux.has-control? (mux.state Lost) "client-1") false "the lost peer's lease is released")
          (and (mux.assert (mux.effects Lost) [["publish" 2 "control" 0]] "the release is published")
            (and (mux.assert (mux.accepted? Again) true "losing the same peer twice is still accepted")
              (and (mux.assert (mux.effects Again) [] "and the second time changes nothing")
                (and (mux.assert (mux.accepted? Stranger) true "losing a peer that never attached is accepted")
                  (mux.assert (mux.state Stranger) Owned "and changes nothing")))))))))))))

\\ A lease window of zero would make every owner instantly stale, which is the
\\ shape of a host that forgot to configure one.
(define mux.trace-clock-validity
  {number --> boolean}
  _ ->
    (and (mux.assert (mux.valid-clock? 0 8000) true "a zero instant is fine")
      (and (mux.assert (mux.valid-clock? 1000 0) false "a zero lease is not a clock")
        (and (mux.assert (mux.owner-fresh? (mux.clk 9000) 1000) true "freshness is measured from the evidence, not from zero")
        (mux.assert (mux.owner-fresh? (mux.clk 9001) 1000) false "and one millisecond past it is stale")))))

(define mux.run-tests
  {number --> boolean}
  N ->
    (and (mux.trace-attach N)
      (and (mux.trace-control-and-events N)
        (and (mux.trace-rejections N)
          (and (mux.trace-barriers N)
            (and (mux.trace-command-contract N)
              (and (mux.trace-lease-preemption N)
                (and (mux.trace-quiet-owner-is-not-evicted N)
                  (and (mux.trace-only-control-renews N)
                    (and (mux.trace-release-clears-evidence N)
                      (and (mux.trace-delivery-classification N)
                        (and (mux.trace-peer-lost-is-idempotent N)
                          (mux.trace-clock-validity N)))))))))))))

(do (mux.run-tests 0)
    (do (print "ALL PASS") (nl)))
