\\ Bifrost/Shen suite for the pure state machine.
(tc +)
(load "specs/mux.shen")

(define mux.assert
  {A --> A --> string --> boolean}
  X X _ -> true
  X Y Label -> (error (cn Label (cn ": expected equality, got " (cn (str X) (cn " / " (str Y)))))))

(define mux.run-tests
  {number --> boolean}
  _ ->
    (let Dim [80 24]
      (let Snap [0 Dim 0 0 false ""]
        (let S0 [[] 0 Snap Dim false false []]
          (let C "client-1"
            (let S1 (mux.attach S0 C)
              (let S2 (mux.acquire-control S1 C)
                (let S3 (mux.apply-delta S2 1)
                  (let S4 (mux.apply-resize S3 2 [100 40])
                    (let S5 (mux.release-control S4 C)
                      (let S6 (mux.apply-control S5 3)
                        (let S7 (mux.apply-exit S6 4)
                          (and
                            (mux.assert (mux.attach-ok? S0 C) true "fresh attach")
                            (and
                              (mux.assert (mux.accept-input? S1 C) false "input before control")
                              (and
                                (mux.assert (mux.acquire-control-ok? S1 C) true "control available")
                                (and
                                  (mux.assert (mux.accept-input? S2 C) true "controller input")
                                  (and
                                    (mux.assert (mux.event-ok? S2 1) true "next event")
                                    (and
                                      (mux.assert (mux.event-ok? S2 2) false "sequence gap")
                                      (and
                                        (mux.assert (mux.accept-input? S6 C) false "input after release")
                                        (and
                                          (mux.assert (mux.accept-input? S7 C) false "input after exit")
                                          (mux.assert (mux.member? C (head S7)) true "client retained"))))))))))))))))))))

(do (mux.run-tests 0)
    (do (print "ALL PASS") (nl)))
