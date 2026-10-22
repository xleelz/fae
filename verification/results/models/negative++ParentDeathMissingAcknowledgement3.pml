// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go#L3
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
	chan child_ParentDeathMissingAcknowledgement30 = [1] of {int}->
	run ParentDeathMissingAcknowledgement3(child_ParentDeathMissingAcknowledgement30)->
	child_ParentDeathMissingAcknowledgement30?0->
stop_process:skip
}

proctype ParentDeathMissingAcknowledgement3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_orphanWaiting162 = [1] of {int}->
	chan child_orphanBlocked151 = [1] of {int}->
	chan child_orphanLost140 = [1] of {int}->
	Chandef done_ch->
	Chandef reusable_ch->
	Chandef groupGone_ch->
	Chandef death_ch->
	run sync_monitor(death_ch)->
	run sync_monitor(groupGone_ch)->
	

	if
	:: 1 > 0 -> 
		reusable_ch.size = 1->
		run async_monitor(reusable_ch)
	:: else -> 
		run sync_monitor(reusable_ch)
	fi->
	run sync_monitor(done_ch)->
	run orphanLost14(death_ch,child_orphanLost140)->
	run receiver(child_orphanLost140)->
	run orphanBlocked15(groupGone_ch,reusable_ch,child_orphanBlocked151)->
	run receiver(child_orphanBlocked151)->
	run orphanWaiting16(reusable_ch,done_ch,child_orphanWaiting162)->
	run receiver(child_orphanWaiting162)->
	death_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:11:2 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:11:2 */->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:12:2 */
	:: done_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:12:2 */->
	:: done_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:12:2 */ -> 
		done_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype orphanLost14(Chandef death_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:14:33 */
	:: death_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:14:33 */->
	:: death_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:14:33 */ -> 
		death_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype orphanBlocked15(Chandef groupGone_ch;Chandef reusable_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:15:49 */
	:: groupGone_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:15:49 */->
	:: groupGone_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:15:49 */ -> 
		groupGone_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:15:61 */
	:: reusable_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:15:61 */
	:: reusable_ch.sync!false -> 
		reusable_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:15:61 */
	fi->
	stop_process: skip->
	child!0
}
proctype orphanWaiting16(Chandef reusable_ch;Chandef done_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:16:44 */
	:: reusable_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:16:44 */->
	:: reusable_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:16:44 */ -> 
		reusable_ch.rcving!false
	fi->
	done_ch.closing?closed /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:16:55 */->
	assert(!closed) /* Close	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.j4T4IY/negative/orphan.go:16:55 */->
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

