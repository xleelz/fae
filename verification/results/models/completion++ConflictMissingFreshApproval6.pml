// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go#L6
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
	chan child_ConflictMissingFreshApproval60 = [1] of {int}->
	run ConflictMissingFreshApproval6(child_ConflictMissingFreshApproval60)->
	child_ConflictMissingFreshApproval60?0->
stop_process:skip
}

proctype ConflictMissingFreshApproval6(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_conflictBadLanding192 = [1] of {int}->
	chan child_conflictBadGates181 = [1] of {int}->
	chan child_conflictBadResolver170 = [1] of {int}->
	Chandef landed_ch->
	Chandef approved_ch->
	Chandef checked_ch->
	Chandef original_ch->
	run sync_monitor(original_ch)->
	run sync_monitor(checked_ch)->
	run sync_monitor(approved_ch)->
	run sync_monitor(landed_ch)->
	run conflictBadResolver17(original_ch,checked_ch,child_conflictBadResolver170)->
	run receiver(child_conflictBadResolver170)->
	run conflictBadGates18(checked_ch,approved_ch,child_conflictBadGates181)->
	run receiver(child_conflictBadGates181)->
	run conflictBadLanding19(landed_ch,child_conflictBadLanding192)->
	run receiver(child_conflictBadLanding192)->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:14:2 */
	:: original_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:14:2 */
	:: original_ch.sync!false -> 
		original_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:14:2 */
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:15:2 */
	:: landed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:15:2 */->
	:: landed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:15:2 */ -> 
		landed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype conflictBadResolver17(Chandef original_ch;Chandef checked_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:17:56 */
	:: original_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:17:56 */->
	:: original_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:17:56 */ -> 
		original_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:17:68 */
	:: checked_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:17:68 */
	:: checked_ch.sync!false -> 
		checked_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:17:68 */
	fi->
	stop_process: skip->
	child!0
}
proctype conflictBadGates18(Chandef checked_ch;Chandef approved_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:18:53 */
	:: checked_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:18:53 */->
	:: checked_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:18:53 */ -> 
		checked_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:18:64 */
	:: approved_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:18:64 */
	:: approved_ch.sync!false -> 
		approved_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:18:64 */
	fi->
	stop_process: skip->
	child!0
}
proctype conflictBadLanding19(Chandef landed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	landed_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:19:44 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/conflict.go:19:44 */->
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

