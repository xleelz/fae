// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go#L2
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_ControlHoldDropsCompletion20 = [1] of {int}->
	run ControlHoldDropsCompletion2(child_ControlHoldDropsCompletion20)->
	child_ControlHoldDropsCompletion20?0->
stop_process:skip
}

proctype ControlHoldDropsCompletion2(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousControlHoldDropsCompletion932 = [1] of {int}->
	chan child_AnonymousControlHoldDropsCompletion831 = [1] of {int}->
	chan child_AnonymousControlHoldDropsCompletion750 = [1] of {int}->
	Chandef collected_ch->
	Chandef completion_ch->
	Chandef held_ch->
	Chandef frozen_ch->
	run sync_monitor(frozen_ch)->
	run sync_monitor(held_ch)->
	run sync_monitor(completion_ch)->
	run sync_monitor(collected_ch)->
	run AnonymousControlHoldDropsCompletion75(completion_ch,collected_ch,frozen_ch,held_ch,child_AnonymousControlHoldDropsCompletion750)->
	run receiver(child_AnonymousControlHoldDropsCompletion750)->
	run AnonymousControlHoldDropsCompletion83(frozen_ch,held_ch,completion_ch,collected_ch,child_AnonymousControlHoldDropsCompletion831)->
	run receiver(child_AnonymousControlHoldDropsCompletion831)->
	run AnonymousControlHoldDropsCompletion93(frozen_ch,held_ch,completion_ch,collected_ch,child_AnonymousControlHoldDropsCompletion932)->
	run receiver(child_AnonymousControlHoldDropsCompletion932)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:10:2 */
	:: collected_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:10:2 */->
	:: collected_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:10:2 */ -> 
		collected_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousControlHoldDropsCompletion75(Chandef completion_ch;Chandef collected_ch;Chandef frozen_ch;Chandef held_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:7:13 */
	:: frozen_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:7:13 */
	:: frozen_ch.sync!false -> 
		frozen_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:7:13 */
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:7:24 */
	:: completion_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:7:24 */
	:: completion_ch.sync!false -> 
		completion_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:7:24 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousControlHoldDropsCompletion83(Chandef frozen_ch;Chandef held_ch;Chandef completion_ch;Chandef collected_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:8:13 */
	:: frozen_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:8:13 */->
	:: frozen_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:8:13 */ -> 
		frozen_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:8:23 */
	:: held_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:8:23 */
	:: held_ch.sync!false -> 
		held_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:8:23 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousControlHoldDropsCompletion93(Chandef frozen_ch;Chandef held_ch;Chandef completion_ch;Chandef collected_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:9:13 */
	:: held_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:9:13 */->
	:: held_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:9:13 */ -> 
		held_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:9:21 */
	:: collected_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:9:21 */
	:: collected_ch.sync!false -> 
		collected_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/control.go:9:21 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

