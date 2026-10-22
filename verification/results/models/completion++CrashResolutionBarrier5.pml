// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go#L5
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
	chan child_CrashResolutionBarrier50 = [1] of {int}->
	run CrashResolutionBarrier5(child_CrashResolutionBarrier50)->
	child_CrashResolutionBarrier50?0->
stop_process:skip
}

proctype CrashResolutionBarrier5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousCrashResolutionBarrier1172 = [1] of {int}->
	chan child_AnonymousCrashResolutionBarrier1061 = [1] of {int}->
	chan child_AnonymousCrashResolutionBarrier960 = [1] of {int}->
	Chandef committed_ch->
	Chandef authority_ch->
	Chandef retired_ch->
	run sync_monitor(retired_ch)->
	run sync_monitor(authority_ch)->
	run sync_monitor(committed_ch)->
	run AnonymousCrashResolutionBarrier96(retired_ch,authority_ch,committed_ch,child_AnonymousCrashResolutionBarrier960)->
	run receiver(child_AnonymousCrashResolutionBarrier960)->
	run AnonymousCrashResolutionBarrier106(retired_ch,authority_ch,committed_ch,child_AnonymousCrashResolutionBarrier1061)->
	run receiver(child_AnonymousCrashResolutionBarrier1061)->
	run AnonymousCrashResolutionBarrier117(authority_ch,committed_ch,retired_ch,child_AnonymousCrashResolutionBarrier1172)->
	run receiver(child_AnonymousCrashResolutionBarrier1172)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:12:2 */
	:: committed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:12:2 */->
	:: committed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:12:2 */ -> 
		committed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCrashResolutionBarrier96(Chandef retired_ch;Chandef authority_ch;Chandef committed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:9:14 */
	:: retired_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:9:14 */
	:: retired_ch.sync!false -> 
		retired_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:9:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCrashResolutionBarrier106(Chandef retired_ch;Chandef authority_ch;Chandef committed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:10:14 */
	:: authority_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:10:14 */
	:: authority_ch.sync!false -> 
		authority_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:10:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCrashResolutionBarrier117(Chandef authority_ch;Chandef committed_ch;Chandef retired_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:14 */
	:: retired_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:14 */->
	:: retired_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:14 */ -> 
		retired_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:25 */
	:: authority_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:25 */->
	:: authority_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:25 */ -> 
		authority_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:38 */
	:: committed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:38 */
	:: committed_ch.sync!false -> 
		committed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/uncertain.go:11:38 */
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

